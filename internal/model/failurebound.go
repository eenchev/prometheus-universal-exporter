package model

import (
	"errors"
	"strconv"
	"unicode/utf8"
)

// A failure says what could not be read, fetched or made, and most say it
// with the part that could not: the token that is no number, the name that
// is no metric's, the URL that did not answer, the message a script raised.
// The part is the target's, the scraper's or the script's to choose, and may
// be as large as the response: a YAML mapping whose key is a sequence of
// 200,000 items is refused with every item in the error, 1.4 MB; a jq rule
// that raises error(.message) fails with the message, and a script that
// raises an exception with it; a redirect's Location is quoted by the error
// of the request it led to. The text goes whole into the log line, into the
// answer to the scraper and into the debug probe's report, and what the
// failure is recognised by (SameFailureText) is kept by the failure log for
// as long as the failure is remembered. limits.max_response_bytes bounds the
// body, not what an error makes of it, which for a quoted or a formatted
// value is several times its size.
//
// So the text of a failure is bounded, whatever it is made of
// (BoundedFailure), at the one place each stage's error leaves by: a
// decoder's where every decoder's does (decode.Decode), and that of every
// other stage of a probe or a static target's scrape where the trip reports
// it (internal/exporter's collect). A site that knows which part of its
// error is the response's cuts that part itself (Quoted, Bare, Shown), and
// keeps what the error says after it.

// MaxFailureBytes is how long the text of a failure may be, and the text it
// is recognised by. The errors of a response that is merely mistaken are far
// under it: a problem with a key or a value of ordinary length is under a
// hundred bytes, and the ten problems a YAML document's error lists under a
// kilobyte.
const MaxFailureBytes = 2000

// CutMark is what stands where a text was cut: how long the whole was, in
// bytes, as QuoteValue says it of a value. In the text a failure is
// recognised by, the length is the mark of something measured (MovingMark),
// as every other size is there: the same failure with a longer or a shorter
// part is one failure to the log.
func CutMark(whole int, recognised bool) string {
	if recognised {
		return "... (" + MovingMark + " bytes)"
	}
	return "... (" + strconv.Itoa(whole) + " bytes)"
}

// HeadOf is how many bytes of text are kept when no more than limit of them
// are: all of a text no longer than limit, and otherwise limit, or up to
// three fewer where the text would be cut inside a character, as QuoteValue
// cuts a value. A text that is no UTF-8 there is cut at limit.
func HeadOf(text string, limit int) int {
	if len(text) <= limit {
		return len(text)
	}
	for head := limit; head > 0 && head > limit-utf8.UTFMax; head-- {
		if utf8.RuneStart(text[head]) {
			return head
		}
	}
	return max(limit, 0)
}

// CutTo is text when it is no longer than limit bytes, and otherwise its
// start and how long it was, in limit bytes or fewer. The cut text is made
// anew, and holds nothing of the text it was cut from.
func CutTo(text string, limit int, recognised bool) string {
	if len(text) <= limit {
		return text
	}
	mark := CutMark(len(text), recognised)
	return text[:HeadOf(text, limit-len(mark))] + mark
}

// Shown is text as a failure's text shows it where it is free text and no
// quoted value — a URL, a line of a traceback — and limit bytes of it are
// enough to tell what it is: the text itself when it is no longer, and
// otherwise its first limit bytes, at a character boundary, and its length,
// as Bare shows a name past 64 bytes. In the text the failure is recognised
// by, the length is the mark.
func Shown(text string, limit int) QuotedValue {
	head := HeadOf(text, limit)
	if head == len(text) {
		return QuotedValue{text: text, same: text}
	}
	// Each is made anew, and holds nothing of the text, which may be a part
	// of the response.
	return QuotedValue{text: text[:head] + CutMark(len(text), false), same: text[:head] + CutMark(len(text), true)}
}

// BoundedFailure is err when its text and the text it is recognised by are
// no longer than MaxFailureBytes, untouched: the same error, of the same
// type, with the same chain. A longer one is a new error of the start of
// that text and its length, recognised by the start of what err was
// recognised by, with the mark for the length: err itself, and so the whole
// text, is let go of, which is why the new error does not wrap it. What err
// was to errors.As is lost with it; what kind of failure it is, which the
// self-metrics count apart (ErrLimitExceeded, ErrMissingValue,
// ErrScriptFailed), is kept.
//
// Whether the recognised text is cut is decided by its own length, not by
// the error's, whose positions and sizes have more digits on one scrape than
// on the next: what a failure is recognised by is made of nothing that
// moves.
//
// An error within the bound costs what reading its two texts costs, and
// nothing else: no error is made for it.
//
// Reading an error must never be what takes a trip down: an error that is a
// nil pointer of its type panics when it is asked for its text, where fmt
// and the log write it as <nil>, so one that cannot be read is handed on as
// it came.
func BoundedFailure(err error) (bounded error) {
	if err == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			bounded = err
		}
	}()
	text, same := err.Error(), SameFailureText(err)
	if len(text) <= MaxFailureBytes && len(same) <= MaxFailureBytes {
		return err
	}
	bounded = SameFailureAs(errors.New(CutTo(text, MaxFailureBytes, false)), CutTo(same, MaxFailureBytes, true))
	for _, kind := range [...]error{ErrLimitExceeded, ErrMissingValue, ErrScriptFailed} {
		if errors.Is(err, kind) {
			bounded = MarkError(bounded, kind)
		}
	}
	return bounded
}
