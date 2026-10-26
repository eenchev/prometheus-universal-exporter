package model

import (
	"errors"
	"fmt"
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
