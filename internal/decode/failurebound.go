package decode

import (
	"errors"
	"strconv"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A decode error says what in the body could not be read, and most say it
// with that part of the body: the token that is no number, the element that
// was not closed, the key that is no key. The part is the target's to choose,
// and may be the whole body: a YAML mapping whose key is a sequence of
// 200,000 items is refused with every item in the error, 1.4 MB, and a line
// of a megabyte that is no carbon line with the line. The text goes whole
// into the log line, into the answer to the scraper and into the debug
// probe's report, and what the failure is recognised by
// (model.SameFailureText) is kept by the failure log for as long as the
// failure is remembered. limits.max_response_bytes bounds the body, not what
// an error makes of it, which for a quoted or a formatted value is several
// times its size.
//
// So the text of a decode error is bounded, whatever the body
// (boundedFailure), at the one place every decoder's error leaves by
// (Decode), and so is the error a decoder reports of the first line it left
// out of a body it decoded (GraphiteReport, PrometheusReport), which is
// logged and remembered as a failure is. A decoder that knows which part of
// its error is the body's cuts that part itself, and keeps what the error
// says after it: the YAML decoder does (yamlPartCut, yamlKeyCut).

// maxFailureBytes is how long the text of a decode error may be, and the
// text it is recognised by. The errors of a body that is merely mistaken are
// far under it: a problem with a key or a value of ordinary length is under a
// hundred bytes, and the ten problems a YAML document's error lists under a
// kilobyte.
const maxFailureBytes = 2000

// cutMark is what stands where a text was cut: how long the whole was, in
// bytes, as model.QuoteValue says it of a value. In the text a failure is
// recognised by, the length is the mark of something measured
// (model.MovingMark), as every other size is there: the same failure with a
// longer or a shorter part is one failure to the log.
func cutMark(whole int, recognised bool) string {
	if recognised {
		return "... (" + model.MovingMark + " bytes)"
	}
	return "... (" + strconv.Itoa(whole) + " bytes)"
}

// headOf is how many bytes of text are kept when no more than limit of them
// are: all of a text no longer than limit, and otherwise limit, or up to
// three fewer where the text would be cut inside a character, as
// model.QuoteValue cuts a value. A text that is no UTF-8 there is cut at
// limit.
func headOf(text string, limit int) int {
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

// cutTo is text when it is no longer than limit bytes, and otherwise its
// start and how long it was, in limit bytes or fewer. The cut text is made
// anew, and holds nothing of the text it was cut from.
func cutTo(text string, limit int, recognised bool) string {
	if len(text) <= limit {
		return text
	}
	mark := cutMark(len(text), recognised)
	return text[:headOf(text, limit-len(mark))] + mark
}

// boundedFailure is err when its text and the text it is recognised by are
// no longer than maxFailureBytes, untouched: the same error, of the same
// type, with the same chain. A longer one is a new error of the start of
// that text and its length, recognised by the start of what err was
// recognised by, with the mark for the length: err itself, and so the whole
// text, is let go of, which is why the new error does not wrap it. What err
// was to errors.As is lost with it; that it is a limit that was exceeded
// (model.ErrLimitExceeded), the one thing a decode error is asked, is kept.
//
// Whether the recognised text is cut is decided by its own length, not by
// the error's, whose positions and sizes have more digits on one scrape than
// on the next: what a failure is recognised by is made of nothing that
// moves.
func boundedFailure(err error) error {
	if err == nil {
		return nil
	}
	text, same := err.Error(), model.SameFailureText(err)
	if len(text) <= maxFailureBytes && len(same) <= maxFailureBytes {
		return err
	}
	bounded := model.SameFailureAs(errors.New(cutTo(text, maxFailureBytes, false)), cutTo(same, maxFailureBytes, true))
	if errors.Is(err, model.ErrLimitExceeded) {
		bounded = model.MarkError(bounded, model.ErrLimitExceeded)
	}
	return bounded
}
