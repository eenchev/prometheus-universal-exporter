package model

import (
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

// An error made with Errorf reads word for word as fmt.Errorf's of the same
// format and arguments does, whatever the verbs, and wraps what that wraps:
// marking where and how large changes no message and no chain.
func TestErrorfReadsAndWrapsAsFmtErrorfDoes(t *testing.T) {
	inner := MarkError(errors.New("value is missing"), ErrMissingValue)
	for _, test := range []struct {
		format        string
		marked, plain []any
	}{
		{"CSV column %q is empty in row %d", []any{"used", Position(3)}, []any{"used", 3}},
		{"metric %q label %q value is %d bytes, longer than limits.max_label_value_length %d", []any{"m", "l", Size(612), 500}, []any{"m", "l", 612, 500}},
		{"file %s was last modified %s ago, longer than request.max_age %s", []any{"/a", Elapsed(90 * time.Second), time.Minute}, []any{"/a", 90 * time.Second, time.Minute}},
		{"metric %q item %d: %w", []any{"m", Position(0), inner}, []any{"m", 0, inner}},
		{"%w for node %d", []any{inner, Position(12)}, []any{inner, 12}},
		{"at line %[2]d, column %[1]d", []any{Position(7), Position(2)}, []any{7, 2}},
		{"row %5d|%-5d|%05d|%x|%v", []any{Position(3), Position(4), Size(5), Size(255), Position(6)}, []any{3, 4, 5, 255, 6}},
		{"took %v, %s", []any{Elapsed(1500 * time.Millisecond), Elapsed(0)}, []any{1500 * time.Millisecond, time.Duration(0)}},
		{"no arguments", nil, nil},
		{"%d of %d: %v and %w", []any{Size(1), Size(2), io.EOF, io.ErrUnexpectedEOF}, []any{1, 2, io.EOF, io.ErrUnexpectedEOF}},
		// An error written with another verb than %w is in the text and
		// not in the chain, alone among the arguments too.
		{"row %d: %v", []any{Position(3), inner}, []any{3, inner}},
		{"row %d: %s", []any{Position(3), io.EOF}, []any{3, io.EOF}},
		{"100%%w of row %d: %v", []any{Position(3), io.EOF}, []any{3, io.EOF}},
		{"row %d: %w and %w", []any{Position(3), io.EOF, inner}, []any{3, io.EOF, inner}},
	} {
		got, want := Errorf(test.format, test.marked...), fmt.Errorf(test.format, test.plain...) //nolint:err113 // the oracle
		if got.Error() != want.Error() {
			t.Errorf("%q: the error is %q, and fmt.Errorf's is %q", test.format, got, want)
		}
		for _, target := range []error{ErrMissingValue, ErrLimitExceeded, io.EOF, io.ErrUnexpectedEOF} {
			if errors.Is(got, target) != errors.Is(want, target) {
				t.Errorf("%q: errors.Is(%v) is %v, and of fmt.Errorf's %v", test.format, target, errors.Is(got, target), errors.Is(want, target))
			}
		}
	}
}

// The text a failure is recognised by leaves out where in the response it
// happened and what was measured, and nothing else: two failures that differ
// in a row, a size or a duration read the same, also through the errors that
// wrap them, and two that differ in a value, a name or a limit do not.
func TestSameFailureTextLeavesOutWhereAndHowLargeAndNothingElse(t *testing.T) {
	emptyIn := func(row int) error {
		return MarkError(Errorf("CSV column %q is empty in row %d", "used", Position(row)), ErrMissingValue)
	}
	notANumber := func(node int, value string) error {
		return Errorf("metric %q node %d: %w", "used", Position(node), fmt.Errorf("value %q is not a number", value))
	}
	wrapped := func(err error) error {
		return fmt.Errorf("collector rows transform failed: %w (the probe ran out of its 10s budget)", MarkError(err, ErrScriptFailed))
	}
	for _, test := range []struct {
		name string
		a, b error
		same bool
	}{
		{"another row", emptyIn(3), emptyIn(7), true},
		{"another row of a wrapped failure", wrapped(emptyIn(3)), wrapped(emptyIn(12345)), true},
		{"another node of the same value", notANumber(2, "n/a"), notANumber(9, "n/a"), true},
		{"another value in the same node", notANumber(2, "n/a"), notANumber(2, "N/A"), false},
		{"another value in another node", wrapped(notANumber(2, "n/a")), wrapped(notANumber(9, "N/A")), false},
		{"another size", Errorf("metric %q has %d labels, more than limits.max_labels_per_metric %d", "m", Size(24), 20), Errorf("metric %q has %d labels, more than limits.max_labels_per_metric %d", "m", Size(31), 20), true},
		{"another limit", Errorf("metric %q has %d labels, more than limits.max_labels_per_metric %d", "m", Size(24), 20), Errorf("metric %q has %d labels, more than limits.max_labels_per_metric %d", "m", Size(24), 22), false},
		{"another metric", Errorf("metric %q has %d labels", "m", Size(24)), Errorf("metric %q has %d labels", "n", Size(24)), false},
		{"another age", Errorf("file %s was last modified %s ago", "/a", Elapsed(time.Minute)), Errorf("file %s was last modified %s ago", "/a", Elapsed(time.Hour)), true},
		{"another file", Errorf("file %s was last modified %s ago", "/a", Elapsed(time.Minute)), Errorf("file %s was last modified %s ago", "/b", Elapsed(time.Minute)), false},
		{"a position within a position", Errorf("%w for node %d", notANumber(1, "x"), Position(4)), Errorf("%w for node %d", notANumber(8, "x"), Position(5)), true},
		{"two failures of one check", JoinProblems(emptyIn(1), notANumber(2, "x")), JoinProblems(emptyIn(4), notANumber(6, "x")), true},
		{"two failures of one check, one of another value", JoinProblems(emptyIn(1), notANumber(2, "x")), JoinProblems(emptyIn(4), notANumber(6, "y")), false},
		{"the same row twice", errors.Join(emptyIn(3), emptyIn(3)), errors.Join(emptyIn(5), emptyIn(6)), true},
		{"a library's error given its text", fmt.Errorf("XML decode: %w", SameFailureAs(errors.New("XML syntax error on line 3: unexpected EOF"), "XML syntax error: unexpected EOF")), fmt.Errorf("XML decode: %w", SameFailureAs(errors.New("XML syntax error on line 90: unexpected EOF"), "XML syntax error: unexpected EOF")), true},
		{"a library's other error", SameFailureAs(errors.New("XML syntax error on line 3: unexpected EOF"), "XML syntax error: unexpected EOF"), SameFailureAs(errors.New("XML syntax error on line 3: invalid character"), "XML syntax error: invalid character"), false},
		{"a number nobody marked", errors.New("received HTTP status 500"), errors.New("received HTTP status 503"), false},
	} {
		if test.a.Error() == test.b.Error() {
			t.Errorf("%s: the two read the same, %q, so the test shows nothing", test.name, test.a)
		}
		a, b := SameFailureText(test.a), SameFailureText(test.b)
		if (a == b) != test.same {
			t.Errorf("%s: recognised by\n%s\nand\n%s\nwant the same: %v", test.name, a, b, test.same)
		}
	}
	// An error without such a part is recognised by its text, and none by
	// nothing.
	plain := fmt.Errorf("probe failed: %w", MarkError(errors.New("row 3 of 7"), ErrLimitExceeded))
	if got := SameFailureText(plain); got != plain.Error() {
		t.Errorf("an error nobody marked is recognised by %q, want its text %q", got, plain)
	}
	if got := SameFailureText(nil); got != "" {
		t.Errorf("no error is recognised by %q", got)
	}
	if SameFailureAs(nil, "x") != nil {
		t.Error("SameFailureAs made an error of none")
	}
	// What a wrapper rewrote is left as it reads.
	rewritten := rewrittenError{emptyIn(3)}
	if got := SameFailureText(rewritten); got != rewritten.Error() {
		t.Errorf("an error whose wrapper rewrote it is recognised by %q, want its text", got)
	}
}

// rewrittenError wraps an error without its text.
type rewrittenError struct{ err error }

func (e rewrittenError) Error() string { return "something else was said" }

func (e rewrittenError) Unwrap() error { return e.err }

// A failure pays for its text when it is read and for its recognised text
// when the log asks: making the error allocates no more than fmt.Errorf
// does, and of a rule that fails on every row only the row that is logged
// is put into words.
func TestErrorfCostsNoMoreThanFmtErrorf(t *testing.T) {
	var sink error
	row := 300
	plain := testing.AllocsPerRun(200, func() {
		sink = fmt.Errorf("CSV column %q is empty in row %d", "used", row) //nolint:err113 // the measure
	})
	marked := testing.AllocsPerRun(200, func() {
		sink = Errorf("CSV column %q is empty in row %d", "used", Position(row))
	})
	wrapping := testing.AllocsPerRun(200, func() {
		sink = fmt.Errorf("metric %q node %d: %w", "used", row, io.EOF) //nolint:err113 // the measure
	})
	markedWrapping := testing.AllocsPerRun(200, func() {
		sink = Errorf("metric %q node %d: %w", "used", Position(row), io.EOF)
	})
	_ = sink
	if marked > plain || markedWrapping > wrapping {
		t.Errorf("Errorf allocates %v times and %v wrapping an error, fmt.Errorf %v and %v: want no more", marked, markedWrapping, plain, wrapping)
	}
}

// nilFailure is an error whose text cannot be asked of a nil pointer.
type nilFailure struct{ text string }

func (f *nilFailure) Error() string { return f.text }

// An error that is a nil pointer of its type, which panics when asked for
// its text, is recognised by what fmt writes of it, on its own and as an
// argument: the failure log asks on the goroutine of the trip, which the
// panic would take down.
func TestSameFailureTextOfANilPointerErrorIsWhatFmtWrites(t *testing.T) {
	var none *nilFailure
	if got, want := SameFailureText(none), fmt.Sprint(error(none)); got != want {
		t.Errorf("a nil pointer error is recognised by %q, want %q", got, want)
	}
	err := Errorf("row %d: %w", Position(3), error(none))
	if got, want := SameFailureText(err), "row #: "+fmt.Sprint(error(none)); got != want {
		t.Errorf("an error that wraps one is recognised by %q, want %q", got, want)
	}
}
