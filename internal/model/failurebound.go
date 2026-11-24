package model

import (
	"errors"
	"strconv"
	"unicode/utf8"
)

// A failure says what could not be read, fetched or made. Most failures
// include the part that failed: the token that is no number, the name that
// is no metric's, the URL that did not answer, the message a script raised.
// The target, the scraper or the script chooses that part, and it may be as
// large as the response. A YAML mapping whose key is a sequence of 200,000
// items is refused with every item in the error, 1.4 MB. A jq rule that
// raises error(.message) fails with the message, and a script that raises an
// exception fails with it. A redirect's Location is quoted by the error of
// the request it led to. The text goes whole into the log line, the answer
// to the scraper and the debug probe's report. The failure log keeps what
// the failure is recognised by (SameFailureText) for as long as it remembers
// the failure. limits.max_response_bytes bounds the body, not what an error
// makes of it, which for a quoted or formatted value is several times its
// size.
//
// So the text of a failure is bounded, whatever it is made of
// (BoundedFailure), at the one place each stage's error passes through. For
// a decoder this is where every decoder's error passes (decode.Decode). For
// every other stage of a probe or a static target's scrape it is where the
// trip reports it (internal/exporter's collect). A site that knows which
// part of its error comes from the response cuts that part itself (Quoted,
// Bare, Shown), and keeps what the error says after it.

// MaxFailureBytes is the maximum length of a failure's text, and of the text
// it is recognised by. Errors of a response that is merely mistaken are far
// below it: a problem with a key or value of ordinary length is under a
// hundred bytes, and the ten problems a YAML document's error lists are
// under a kilobyte.
const MaxFailureBytes = 2000

// CutMark is what stands where a text was cut: the length of the whole, in
// bytes, as QuoteValue gives it for a value. In the text a failure is
// recognised by, the length is the mark of something measured (MovingMark),
// like every other size there. So the same failure with a longer or shorter
// part is one failure to the log.
func CutMark(whole int, recognised bool) string {
	if recognised {
		return "... (" + MovingMark + " bytes)"
	}
	return "... (" + strconv.Itoa(whole) + " bytes)"
}

// HeadOf returns how many bytes of text are kept when at most limit are
// kept: all of a text no longer than limit, and otherwise limit, or up to
// three fewer where the cut would fall inside a character, as QuoteValue
// cuts a value. Text that is not UTF-8 at that point is cut at limit.
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

// Shown returns text as a failure's text shows it, where it is free text
// rather than a quoted value (a URL, a line of a traceback) and limit bytes
// are enough to tell what it is. A text no longer than limit is shown as is.
// A longer one is cut to its first limit bytes, at a character boundary, and
// followed by its length, as Bare shows a name past 64 bytes. In the text
// the failure is recognised by, the length is the mark.
func Shown(text string, limit int) QuotedValue {
	head := HeadOf(text, limit)
	if head == len(text) {
		return QuotedValue{text: text, same: text}
	}
	// Each is made anew, and holds nothing of the text, which may be a part
	// of the response.
	return QuotedValue{text: text[:head] + CutMark(len(text), false), same: text[:head] + CutMark(len(text), true)}
}

// BoundedFailure returns err untouched when its text and the text it is
// recognised by are no longer than MaxFailureBytes: the same error, of the
// same type, with the same chain. For a longer one it returns a new error
// with the start of that text and its length. The new error is recognised
// by the start of what err was recognised by, with the mark for the length.
// err itself, and so its whole text, is let go of, which is why the new
// error does not wrap it. What err was to errors.As is lost with it. Two
// things are kept: the kind of failure, which the self-metrics count
// separately (ErrLimitExceeded, ErrMissingValue, ErrScriptFailed), and what
// a probe's caller is shown of it (CallerText), cut to the same bound.
//
// Whether the recognised text is cut depends on its own length, not on the
// error's. The error's positions and sizes can have more digits on one
// scrape than on the next, but what a failure is recognised by contains
// nothing that moves.
//
// An error within the bound costs only reading its two texts. No new error
// is made for it.
//
// Reading an error must never take a trip down. An error that is a nil
// pointer of its type panics when asked for its text, while fmt and the log
// write it as <nil>. So an error that cannot be read is passed on as it
// came.
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
	// What a probe's caller is shown of err is taken before err is let go
	// of, and kept with the new error: of its text alone, CallerText could
	// tell no value of the answer, and no operator's detail, from the rest.
	if caller := CallerText(err); caller != text {
		bounded = WithOperatorDetail(bounded, CutTo(caller, MaxFailureBytes, false))
	}
	return bounded
}
