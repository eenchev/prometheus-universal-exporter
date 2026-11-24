package decode

import "github.com/eenchev/prometheus-universal-exporter/internal/model"

// A decode error says what in the body could not be read, and most errors
// quote that part of the body: the token that is not a number, the element
// that was not closed, the key that is not a key. The target chooses that
// part, and it may be the whole body. A YAML mapping whose key is a sequence
// of 200,000 items is refused with every item in the error, 1.4 MB, and a
// line of a megabyte that is not a carbon line is refused with the line. The
// text goes whole into the log line, into the answer to the scraper and into
// the debug probe's report. The failure log keeps what the failure is
// recognised by (model.SameFailureText) for as long as it remembers the
// failure. limits.max_response_bytes bounds the body, not what an error
// makes of it, which for a quoted or a formatted value is several times its
// size.
//
// So the text of a decode error is bounded, whatever the body
// (boundedFailure), at Decode, the one place every decoder's error leaves
// by. So is the error a decoder reports for the first line it left out of a
// body it decoded (GraphiteReport, PrometheusReport), which is logged and
// remembered like a failure. A decoder that knows which part of its error
// comes from the body cuts that part itself, and keeps what the error says
// after it. The YAML decoder does this (yamlPartCut, yamlKeyCut).
//
// The bound is the one for every stage's failure, and is defined where every
// stage can use it (model.BoundedFailure). The code below names it here as
// the decoders do.

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

// boundedFailure returns err untouched when its text and the text it is
// recognised by are no longer than maxFailureBytes. Otherwise it returns a
// new error of the start of each and its length, which keeps that it is a
// limit that was exceeded and lets go of err (model.BoundedFailure).
func boundedFailure(err error) error { return model.BoundedFailure(err) }
