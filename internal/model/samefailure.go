package model

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// A failure's text says where in the response it happened — row 3, node 7,
// line 12 — and how large the thing was that went over a limit, because that
// is what the operator goes and looks at. The log, though, tells a failure
// that keeps happening from a new one by its text (internal/exporter's
// failure log), and a table whose empty cell is in row 3 on one scrape and in
// row 7 on the next, or a label 612 bytes long and then 640, is the same
// failure to whoever reads the log: logged in full on every scrape, it buries
// the rest, is never summed up as a repeat, and its recovery counts only
// since it last moved.
//
// So an error that carries such a part says which part of its text it is, by
// the type of the argument it is made with (Position, Size, Elapsed, through
// Errorf), and SameFailureText gives the text the failure is recognised by:
// the whole text with those parts replaced by a fixed mark. The text itself,
// which is what is logged and what a scraper is answered, is unchanged. No
// message is searched for numbers: a value the response held, as in `value
// "n/a" is not a number`, is part of what the failure is, and a number the
// configuration gave is the same on every scrape.
//
// A value of the response that an error shows is shown by its start when it
// is long (QuotedValue, made by Quoted and Bare), with its length after it:
// the start is part of what the failure is, and the length, a size measured,
// is not.

// Position is a number in a failure's text that says where in the response
// the failure happened: a row, a node, an item, a line, a column.
type Position int

// Size is a number in a failure's text that was measured of the response: a
// length in bytes, or how many of something it held.
type Size int64

// Elapsed is a time in a failure's text that was measured: how long
// something ran, or how old it is.
type Elapsed time.Duration

func (e Elapsed) String() string { return time.Duration(e).String() }

// MovingMark stands in a failure's recognised text for each part that says
// where or how large.
const MovingMark = "#"

// QuotedValue is a value of the response as a failure's text shows it,
// given to Errorf: no more of it than its start, with how long it was, so
// that a value the target made as long as its response leaves the text short
// and what the text says after the value read. In the text the failure is
// recognised by, the length is the mark: the same failure with a longer or a
// shorter value that starts the same is one failure to the log.
//
// It is made of the value once, and holds nothing of it but the start it
// shows: an error keeps its arguments for as long as it is kept.
type QuotedValue struct {
	// text is what the failure's text shows of the value, and same what the
	// failure is recognised by.
	text, same string
}

// Quoted is text as QuoteValue quotes it: quoted, and past 64 bytes cut to
// its first 64, at a character boundary, with its length after the quotes.
// The whole of a long text is neither quoted nor copied to make it.
func Quoted[T ~string | ~[]byte](text T) QuotedValue {
	head := shownBytes(text)
	return shownStart(strconv.Quote(string(text[:head])), head, len(text))
}

// Bare is text as it is, for a name the failure's text writes without
// quotes, cut as Quoted cuts a value: past 64 bytes to its first 64 and its
// length.
func Bare[T ~string | ~[]byte](text T) QuotedValue {
	head := shownBytes(text)
	// A copy: the text may be a part of the response.
	return shownStart(strings.Clone(string(text[:head])), head, len(text))
}

// ShownAs is what a failure's text shows of the response where that is more
// than one value, as a series named by its labels is: the text made of the
// values as each is shown (String), and the text it is recognised by, made
// of what each is recognised by (Same).
func ShownAs(text, same string) QuotedValue { return QuotedValue{text: text, same: same} }

// shownBytes is how many bytes of a value are shown: all of one no longer
// than maxQuotedValue, and otherwise that many, or fewer where the value
// would be cut inside a character, as QuoteValue cuts it.
func shownBytes[T ~string | ~[]byte](text T) int {
	if len(text) <= maxQuotedValue {
		return len(text)
	}
	cut := maxQuotedValue
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return cut
}

// shownStart is a value of whole bytes shown by start, which is what is
// shown of its first head bytes.
func shownStart(start string, head, whole int) QuotedValue {
	if head == whole {
		return QuotedValue{text: start, same: start}
	}
	return QuotedValue{text: start + "... (" + strconv.Itoa(whole) + " bytes)", same: start + "... (" + MovingMark + " bytes)"}
}

// String is the value as a failure's text shows it.
func (q QuotedValue) String() string { return q.text }

// Same is the value as the failure is recognised by it: String, with the
// mark in place of the length of a value that was cut.
func (q QuotedValue) Same() string { return q.same }

// Format writes the value as String gives it whatever verb formats it: a %q
// would quote the quotes.
func (q QuotedValue) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, q.text) }

// movingPart is written as MovingMark whatever verb formats it.
type movingPart struct{}

func (movingPart) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, MovingMark) }

// movingError is an error made by Errorf: the format and the arguments, of
// which the text is made when it is asked for, and the text the failure is
// recognised by when the failure log asks. A rule that fails on every row of
// a table makes an error a row, of which one is logged: the others are
// counted and dropped, and cost no text.
type movingError struct {
	format string
	args   []any
	// wraps is the error among the arguments, which the error wraps; of
	// several, what fmt.Errorf makes of the format, which wraps those its
	// %w verbs name.
	wraps error
}

func (e *movingError) Error() string { return fmt.Errorf(e.format, e.args...).Error() }

func (e *movingError) Unwrap() error { return e.wraps }

// same is the error's text with each Position, Size and Elapsed replaced by
// the mark, each QuotedValue by what it is recognised by, and each error
// among the arguments by its own recognised text.
func (e *movingError) same() string {
	args := make([]any, len(e.args))
	for i, arg := range e.args {
		switch arg := arg.(type) {
		case Position, Size, Elapsed:
			args[i] = movingPart{}
		case QuotedValue:
			args[i] = QuotedValue{text: arg.same, same: arg.same}
		case error:
			args[i] = sameText(SameFailureText(arg))
		default:
			args[i] = arg
		}
	}
	return fmt.Errorf(e.format, args...).Error()
}

// sameText is an error argument's recognised text, as the error it stands
// for in the format.
type sameText string

func (s sameText) Error() string { return string(s) }

// Errorf is fmt.Errorf for a failure whose text says where in the response
// it happened or how large something measured was: those arguments are
// given as a Position, a Size or an Elapsed, which are formatted as the
// numbers and the duration they are, and a value of the response it shows as
// a QuotedValue. The error reads as fmt.Errorf's does, and wraps what
// fmt.Errorf's would: the errors the format names with %w, and no error
// written with another verb.
//
// The text is made when it is read, not here, so the arguments are values
// that stay what they are: names, numbers, texts and errors.
func Errorf(format string, args ...any) error {
	e := &movingError{format: format, args: args}
	wrapping := strings.Count(format, "%w")
	if wrapping == 0 {
		return e
	}
	// One %w and one error among the arguments is nearly every failure
	// that wraps, and is told without making the text. Anything else — a
	// second %w, a second error, a %% that may stand before a w — is
	// fmt.Errorf's to say.
	var inner error
	errs := 0
	for _, arg := range args {
		if err, ok := arg.(error); ok {
			inner, errs = err, errs+1
		}
	}
	if wrapping == 1 && errs == 1 && !strings.Contains(format, "%%") {
		e.wraps = inner
	} else {
		e.wraps = fmt.Errorf(format, args...)
	}
	return e
}

// sameAsError is an error whose recognised text is given, for an error made
// elsewhere, as a library's, that has its position in its text.
type sameAsError struct {
	err  error
	same string
}

func (e *sameAsError) Error() string { return e.err.Error() }

func (e *sameAsError) Unwrap() error { return e.err }

// SameFailureAs gives err the text it is recognised by, for an error whose
// text was made elsewhere with a position in it: same is that text without
// the position. The message and the chain are unchanged.
func SameFailureAs(err error, same string) error {
	if err == nil {
		return nil
	}
	return &sameAsError{err: err, same: same}
}

// SameFailureText is the text a failure is recognised by: its text, with the
// parts that say where in the response it happened and what was measured
// replaced by a fixed mark, so that two failures differing only in those
// read the same. An error without such a part is recognised by its text as
// it is.
//
// The errors that wrap one that has such a part have written its text into
// theirs, so that text is replaced where it stands in the whole; a wrapper
// that rewrote it leaves the whole as it is.
func SameFailureText(err error) (same string) {
	// An error that is a nil pointer of its type panics when asked for its
	// text; fmt writes such a one as <nil>, and so the failure is recognised
	// by what fmt makes of it rather than taking the trip down with it.
	defer func() {
		if recover() != nil {
			same = fmt.Sprint(err)
		}
	}()
	switch e := err.(type) { //nolint:errorlint // the error itself; those it wraps are walked below
	case nil:
		return ""
	case *movingError:
		return e.same()
	case *sameAsError:
		return e.same
	}
	return withoutMovingParts(err, err.Error())
}

// withoutMovingParts replaces in text, the text of an error that wraps err,
// what err and the errors it wraps have that moves.
func withoutMovingParts(err error, text string) string {
	switch e := err.(type) { //nolint:errorlint // this is the walk down the chain
	case *movingError:
		return strings.Replace(text, e.Error(), e.same(), 1)
	case *sameAsError:
		return strings.Replace(text, e.Error(), e.same, 1)
	case interface{ Unwrap() error }:
		if inner := e.Unwrap(); inner != nil {
			return withoutMovingParts(inner, text)
		}
	case interface{ Unwrap() []error }:
		for _, inner := range e.Unwrap() {
			if inner != nil {
				text = withoutMovingParts(inner, text)
			}
		}
	}
	return text
}
