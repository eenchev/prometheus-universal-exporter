package fetch

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// urlFailures are the errors a failed HTTP fetch ends with for a request of
// address, as Go's client and the fetch make them: the request refused by
// the network, a redirect to the address refused by the target policy, which
// writes the URL a second time, and the same wrapped by what a retry cut
// short says after it.
func urlFailures(address string) []error {
	refused := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 80}, Err: errors.New("connect: connection refused")}
	redirect := &url.Error{Op: "Get", URL: address, Err: fmt.Errorf("redirect to %s refused: %w", RedactURLString(address, MaskQueryValues), fmt.Errorf("target %w: its address is in request.denied_targets", ErrTargetRefused))}
	return []error{
		&url.Error{Op: "Get", URL: address, Err: refused},
		fmt.Errorf("HTTP request failed: %w", &url.Error{Op: "Get", URL: address, Err: refused}),
		fmt.Errorf("HTTP request failed: %w", redirect),
		fmt.Errorf("HTTP request failed: %w (the wait before retrying was cut short: %w)", &url.Error{Op: "Post", URL: address, Err: errors.New("stopped after 10 redirects")}, errors.New("context deadline exceeded")),
		fmt.Errorf("invalid target: %w", &url.Error{Op: "parse", URL: address, Err: errors.New("invalid port \":x\" after host")}),
	}
}

// A failed fetch whose URL is 512 bytes or fewer ends with the error it
// did, the very one: for generated URLs of every length up to that, with and
// without a query, in every shape a failed HTTP fetch's error has, what
// leaves the fetch is what RedactURLErrors made of the error, untouched.
func TestAFetchErrorOfAURLOfOrdinaryLengthIsTheErrorItWas(t *testing.T) {
	random := rand.New(rand.NewPCG(36, 5))
	for range alloctest.UnlessRaced(2000, 400) {
		address := "http://db.internal:9100/" + strings.Repeat([]string{"p", "é", "%20"}[random.IntN(3)], random.IntN(160))
		if random.IntN(2) == 0 {
			address += "?tenant=" + strings.Repeat("t", random.IntN(40)) + "&token=s3cret"
		}
		if len(address) > shownURLBytes {
			address = address[:shownURLBytes]
		}
		for _, made := range urlFailures(address) {
			was := RedactURLErrors(made)
			if got := shortURLErrors(was); got != was { //nolint:errorlint // the very error
				t.Fatalf("the error of a URL of %d bytes is not the error it was:\n%s\nwas\n%s", len(address), got, was)
			}
		}
	}
	if shortURLErrors(nil) != nil {
		t.Error("no error became an error")
	}
	if plain := errors.New("gRPC call failed: UNAVAILABLE"); shortURLErrors(plain) != plain { //nolint:errorlint // the very error
		t.Error("an error that quotes no URL was changed")
	}
}

// A failed fetch whose URL is longer than 512 bytes says why it failed: the
// URL is shown by its first 512 bytes and its length, where the error quotes
// it and where the refusal of a redirect writes it again, and what the error
// says after it, the reason, is whole, in an error under 1,400 bytes. It was
// as long as the URL — eight kilobytes for a scraper's target, a megabyte
// for a redirect's Location — and, cut at the 2,000 bytes of any failure,
// nothing but the start of the URL. The credentials of the URL are withheld
// as they were, and a query cut through is shown as far as it goes; the
// error unwraps to what it did; it is recognised with the mark in place of
// the length, so a longer URL that starts alike is the same failure, and
// with the mark in place of the address the connection was made from, as
// every fetch's error is. Shown again, the error is as it is.
func TestAFetchErrorOfALongURLShowsItsStartAndSaysWhy(t *testing.T) {
	path := strings.Repeat("p", alloctest.UnlessRaced(1<<20, 1<<17))
	for _, length := range []int{shownURLBytes + 1 - len("http://db.internal:9100/"), 8000, len(path)} {
		address := "http://db.internal:9100/" + path[:length]
		head := address[:shownURLBytes]
		for i, made := range urlFailures(address) {
			err := shortURLErrors(RedactURLErrors(made))
			shown := fmt.Sprintf("%q... (%d bytes)", head, len(address))
			want := strings.NewReplacer(fmt.Sprintf("%q", address), shown, address, fmt.Sprintf("%s... (%d bytes)", head, len(address))).Replace(made.Error())
			if text := err.Error(); text != want || len(text) > 1400 {
				t.Errorf("failure %d of a URL of %d bytes reads, in %d bytes,\n%.300s ... %s\nwant\n%.300s ... %s", i, len(address), len(text), text, text[max(0, len(text)-200):], want, want[max(0, len(want)-200):])
			}
			if same, want := model.SameFailureText(err), strings.ReplaceAll(want, fmt.Sprintf("(%d bytes)", len(address)), "(# bytes)"); same != want {
				t.Errorf("failure %d of a URL of %d bytes is recognised by\n%.300s ... %s", i, len(address), same, same[max(0, len(same)-200):])
			}
			var quoting *url.Error
			if !errors.As(err, &quoting) || quoting.URL != address || errors.Is(made, ErrTargetRefused) != errors.Is(err, ErrTargetRefused) {
				t.Errorf("failure %d of a URL of %d bytes no longer unwraps to what it did", i, len(address))
			}
			if again := shortURLErrors(RedactURLErrors(err)); again.Error() != err.Error() || model.SameFailureText(again) != model.SameFailureText(err) {
				t.Errorf("failure %d of a URL of %d bytes, shown again, reads\n%.300s", i, len(address), again)
			}
		}
	}
	// A credential of the URL, and the address the connection was made
	// from.
	secret := "http://user:pw@db.internal:9100/" + path[:600] + "?token=s3cret&tenant=" + path[:300]
	reset := &net.OpError{Op: "read", Net: "tcp", Source: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 53412}, Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 80}, Err: errors.New("read: connection reset by peer")}
	err := sameFetchFailure(shortURLErrors(RedactURLErrors(fmt.Errorf("HTTP request failed: %w", &url.Error{Op: "Get", URL: secret, Err: reset}))))
	redacted := "http://redacted:redacted@db.internal:9100/" + path[:600] + "?token=<redacted>&tenant=<redacted>"
	if text, want := err.Error(), fmt.Sprintf("HTTP request failed: Get %q... (%d bytes): read tcp 10.0.0.1:53412->10.0.0.2:80: read: connection reset by peer", redacted[:shownURLBytes], len(redacted)); text != want || strings.Contains(text, "s3cret") || strings.Contains(text, ":pw@") {
		t.Errorf("the error of a long URL with credentials reads\n%s\nwant\n%s", text, want)
	}
	if same, want := model.SameFailureText(err), fmt.Sprintf("HTTP request failed: Get %q... (# bytes): read tcp #->10.0.0.2:80: read: connection reset by peer", redacted[:shownURLBytes]); same != want {
		t.Errorf("the error of a long URL with credentials is recognised by\n%s\nwant\n%s", same, want)
	}
	// A URL of characters of several bytes is cut between two.
	wide := "http://db.internal:9100/" + strings.Repeat("日", 400)
	if text := shortURLErrors(&url.Error{Op: "Get", URL: wide, Err: errors.New("refused")}).Error(); text != fmt.Sprintf("Get %q... (%d bytes): refused", wide[:model.HeadOf(wide, shownURLBytes)], len(wide)) {
		t.Errorf("a URL of wide characters is shown as %.100q ... %q", text, text[max(0, len(text)-60):])
	}
}

// How a request ended is in a debug probe's report in no more than the 2,000
// bytes of any failure: an error as long as what the target sent, or a
// status line as long as its headers may be, is recorded by its start and
// its length, and so is the error a body broke off with, after the status.
// They were recorded whole, a megabyte in a line of the report. An outcome
// of ordinary length is recorded as it was.
func TestHowARequestEndedIsRecordedInNoMoreThanAFailureHolds(t *testing.T) {
	long := strings.Repeat("s", alloctest.UnlessRaced(1<<20, 1<<17))
	for _, tc := range []struct{ outcome, broke, want string }{
		{"200 OK", "", "200 OK"},
		{"error: Get \"http://db.internal/\": connection refused", "", "error: Get \"http://db.internal/\": connection refused"},
		{"200 OK", "unexpected EOF", "200 OK, then the body broke off: unexpected EOF"},
		{"error: malformed HTTP response \"" + long + "\"", "", ("error: malformed HTTP response \"" + long)[:model.MaxFailureBytes-len(fmt.Sprintf("... (%d bytes)", len(long)+33))] + fmt.Sprintf("... (%d bytes)", len(long)+33)},
		{"200 " + long, "", ("200 " + long)[:model.MaxFailureBytes-len(fmt.Sprintf("... (%d bytes)", len(long)+4))] + fmt.Sprintf("... (%d bytes)", len(long)+4)},
		{"200 OK", "bad chunk " + long, "200 OK, then the body broke off: " + ("bad chunk " + long)[:model.MaxFailureBytes-len(fmt.Sprintf("... (%d bytes)", len(long)+10))] + fmt.Sprintf("... (%d bytes)", len(long)+10)},
	} {
		ctx, trace := WithRequestTrace(t.Context())
		traceRequest(ctx, "GET", "http://db.internal/", nil, "", false)
		traceOutcome(ctx, tc.outcome)
		if tc.broke != "" {
			traceBodyError(ctx, errors.New(tc.broke))
		}
		if got := trace.Requests()[0].Outcome; got != tc.want {
			t.Errorf("a request that ended with %d bytes is recorded in %d, %.80q ... %q, want %.80q ... %q", len(tc.outcome)+len(tc.broke), len(got), got, got[max(0, len(got)-40):], tc.want, tc.want[max(0, len(tc.want)-40):])
		}
	}
}
