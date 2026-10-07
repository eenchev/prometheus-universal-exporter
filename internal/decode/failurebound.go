package decode

import "github.com/eenchev/prometheus-universal-exporter/internal/model"

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
//
// The bound is that of every stage's failure, and is made where every stage
// can use it (model.BoundedFailure): what follows names it here as the
// decoders do.

// maxFailureBytes is how long the text of a decode error may be, and the
// text it is recognised by (model.MaxFailureBytes).
const maxFailureBytes = model.MaxFailureBytes

// cutMark is what stands where a text was cut: how long the whole was, in
// bytes, and in the text a failure is recognised by the mark of something
// measured (model.CutMark).
func cutMark(whole int, recognised bool) string { return model.CutMark(whole, recognised) }

// headOf is how many bytes of text are kept when no more than limit of them
// are, the cut between two characters (model.HeadOf).
func headOf(text string, limit int) int { return model.HeadOf(text, limit) }

// boundedFailure is err when its text and the text it is recognised by are
// no longer than maxFailureBytes, untouched, and otherwise a new error of
// the start of each and its length, which keeps that it is a limit that was
// exceeded and lets go of err (model.BoundedFailure).
func boundedFailure(err error) error { return model.BoundedFailure(err) }
