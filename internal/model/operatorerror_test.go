package model

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// An error marked as holding the operator's detail reads whole, and is
// recognised as it was, while a caller is shown its own text in its place,
// with what wraps it kept; an unmarked error is shown to a caller whole.
func TestCallerTextReplacesTheOperatorsDetail(t *testing.T) {
	inner := Errorf("the command exited with %d; its stderr began: %s", 1, Shown("secret line", 100))
	marked := WithOperatorDetail(inner, "the command exited with 1")
	wrapped := fmt.Errorf("on host: %w", marked)
	if wrapped.Error() != "on host: the command exited with 1; its stderr began: secret line" || SameFailureText(wrapped) != SameFailureText(fmt.Errorf("on host: %w", inner)) {
		t.Errorf("the error reads %q, recognised by %q", wrapped, SameFailureText(wrapped))
	}
	if got := CallerText(wrapped); got != "on host: the command exited with 1" {
		t.Errorf("the caller is shown %q", got)
	}
	if !errors.Is(wrapped, inner) {
		t.Error("the marked error does not wrap its own")
	}
	if got := CallerText(errors.New("plain")); got != "plain" {
		t.Errorf("an unmarked error is shown %q", got)
	}
	if WithOperatorDetail(nil, "x") != nil {
		t.Error("a nil error is marked")
	}
}

// A value of the answer that a failure quotes is withheld from the caller,
// wherever the error that quotes it is in the chain, and the rest of the
// text is shown as it reads; a value given a caller's text is shown that.
func TestCallerTextWithholdsTheValuesOfTheAnswer(t *testing.T) {
	inner := Errorf("expected float as value, got %s", Quoted("AWS_SECRET=abc"))
	wrapped := fmt.Errorf("decoding the response: %w", Errorf("line %d: %w", Position(3), inner))
	if want := "decoding the response: line 3: expected float as value, got " + WithheldMark; CallerText(wrapped) != want {
		t.Errorf("the caller is shown %q, want %q", CallerText(wrapped), want)
	}
	if !strings.Contains(wrapped.Error(), "AWS_SECRET=abc") {
		t.Errorf("the error reads %q; want the value in it", wrapped)
	}
	typed := Errorf("python transform failed: %s", ShownAs("Traceback...\nValueError: abc", "same").ForCaller("ValueError"))
	if got := CallerText(typed); got != "python transform failed: ValueError" {
		t.Errorf("a value given a caller's text is shown %q", got)
	}
	both := errors.Join(Errorf("a %s", Quoted("x")), WithOperatorDetail(errors.New("secret"), "kept"))
	if got := CallerText(both); got != "a "+WithheldMark+"\nkept" {
		t.Errorf("a joined error is shown %q", got)
	}
}

// A wrapper that rewrote the text of an error with something to withhold
// leaves nothing to tell it apart by: the caller is answered that the
// failure is in the log, not the rewritten text.
func TestCallerTextOfARewrittenErrorShowsNothingOfIt(t *testing.T) {
	inner := Errorf("got %s", Quoted("secret"))
	rewritten := &rewording{err: inner}
	if got := CallerText(rewritten); got != WithheldFailure {
		t.Errorf("the caller is shown %q, want %q", got, WithheldFailure)
	}
}

type rewording struct{ err error }

func (e *rewording) Error() string { return strings.ToUpper(e.err.Error()) }

func (e *rewording) Unwrap() error { return e.err }

// An error over the bound is a new error of the start of its text, which no
// longer wraps what it was: what the caller is shown of it is kept with it,
// so that neither a value of the answer nor the operator's detail reaches
// the caller through the cut.
func TestABoundedFailureKeepsWhatTheCallerIsShown(t *testing.T) {
	long := strings.Repeat("x", MaxFailureBytes)
	for name, err := range map[string]error{
		"operator's detail": WithOperatorDetail(errors.New("stderr: secret "+long), "the command failed"),
		"value":             Errorf("got %s; then %s", Quoted("secret"), long),
	} {
		bounded := BoundedFailure(fmt.Errorf("wrapped: %w", err))
		if len(bounded.Error()) > MaxFailureBytes+64 || !strings.Contains(bounded.Error(), "secret") {
			t.Errorf("%s: the bounded error reads %d bytes", name, len(bounded.Error()))
		}
		if got := CallerText(bounded); strings.Contains(got, "secret") || !strings.HasPrefix(got, "wrapped: ") {
			t.Errorf("%s: the caller is shown %.200q", name, got)
		}
	}
	plain := BoundedFailure(errors.New(long + long))
	if got := CallerText(plain); got != plain.Error() {
		t.Errorf("a bounded error with nothing to withhold is shown %.100q", got)
	}
}

// An error that rewrites the text of the one it wraps, as a redaction of
// its URLs does, says itself what a caller is told of it: the caller's text
// of the wrapped error, rewritten as its own text was.
func TestCallerTextOfATextRewriterIsTheRewrittenCallerText(t *testing.T) {
	inner := fmt.Errorf("Get https://a/b?token=s: %w", WithOperatorDetail(errors.New("stderr: secret"), "the command failed"))
	rewritten := &rewriter{err: inner, from: "token=s", to: "token=xxx"}
	if rewritten.Error() != "Get https://a/b?token=xxx: stderr: secret" {
		t.Fatalf("the error reads %q", rewritten)
	}
	if got := CallerText(fmt.Errorf("wrapped: %w", rewritten)); got != "wrapped: Get https://a/b?token=xxx: the command failed" {
		t.Errorf("the caller is told %q", got)
	}
}

type rewriter struct {
	err      error
	from, to string
}

func (e *rewriter) Error() string { return e.RewriteText(e.err.Error()) }

func (e *rewriter) Unwrap() error { return e.err }

func (e *rewriter) RewriteText(text string) string { return strings.ReplaceAll(text, e.from, e.to) }
