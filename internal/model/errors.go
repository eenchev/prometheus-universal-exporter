package model

import (
	"errors"
	"fmt"
	"strings"
)

// Some failures are counted in their own self-metrics: a response or file over
// the size limit (http_exporter_series_limit_exceeded_total), a metric value
// the response did not contain (http_exporter_missing_keys_total) and a Python
// script that failed (http_exporter_script_errors_total). Which counter an
// error raises is decided by what the error is, marked where it is made, and
// read with errors.Is — never by searching its text, which a reworded message
// or an unrelated error that happens to say "missing" would get wrong.
var (
	ErrLimitExceeded = errors.New("limit exceeded")
	ErrMissingValue  = errors.New("value missing")
	ErrScriptFailed  = errors.New("script failed")
)

// kindError marks an error with its kind while keeping its message and chain.
type kindError struct {
	err  error
	kind error
}

func (e *kindError) Error() string { return e.err.Error() }

func (e *kindError) Unwrap() error { return e.err }

func (e *kindError) Is(target error) bool { return target == e.kind }

// MarkError marks err as a failure of the given kind. The message is unchanged.
func MarkError(err, kind error) error {
	if err == nil {
		return nil
	}
	return &kindError{err: err, kind: kind}
}

// MaxReportedProblems is how many problems a Problems error spells out; the
// rest are counted.
const MaxReportedProblems = 20

// Problems is every mistake a check found, in the order it found them, so a
// configuration with several is fixed in one pass rather than one run per
// mistake. Its text has one problem per line: the first MaxReportedProblems,
// then how many more there are. Unwrap gives them all.
type Problems []error

func (p Problems) Error() string {
	var b strings.Builder
	for i, err := range p {
		if i == MaxReportedProblems {
			more := len(p) - i
			fmt.Fprintf(&b, "\nand %d more problem%s", more, plural(more))
			break
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(err.Error())
	}
	return b.String()
}

func (p Problems) Unwrap() []error { return p }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// JoinProblems collects errors into one: nil when there are none, the error
// itself when there is one, and otherwise Problems holding each, with the
// problems of a Problems among them spliced in, in order, and nils dropped.
func JoinProblems(errs ...error) error {
	var all Problems
	for _, err := range errs {
		// Only a Problems itself is spliced in: one wrapped with context
		// keeps it, as one problem.
		if nested, ok := err.(Problems); ok { //nolint:errorlint // see above

			all = append(all, nested...)
		} else if err != nil {
			all = append(all, err)
		}
	}
	switch len(all) {
	case 0:
		return nil
	case 1:
		return all[0]
	}
	return all
}

// What a probe's caller is answered of a failure is less than the operator
// is shown. The caller may be anyone the probe endpoint answers, and may
// point a collector at a target, a file or a command its parameters let
// through; what the target answered is the operator's, as what it means is
// the series. So the caller is answered a failure's text (CallerText) with
// every value of the answer it quotes (QuotedValue) withheld, and with what
// an error marked as the operator's (WithOperatorDetail) holds replaced by
// what the caller may be told of it; the log shows the whole text, and so
// do the debug report and, with --web.debug=full, the status page.

// WithheldMark is what a caller is shown in place of a value of the answer
// that a failure quotes.
const WithheldMark = "[withheld; in the exporter's log]"

// WithheldFailure is what a caller is answered of a failure whose text could
// not be told apart from what it withholds: a wrapper rewrote the text of an
// error that holds the operator's detail, so nothing of it is shown.
const WithheldFailure = "what failed is in the exporter's log"

// operatorError is an error whose text holds what is for the exporter's
// operator alone, such as what a remote command wrote to stderr or what a
// database said: the log shows the whole text, and the caller of a probe
// only caller.
type operatorError struct {
	err    error
	caller string
}

func (e *operatorError) Error() string { return e.err.Error() }

func (e *operatorError) Unwrap() error { return e.err }

// WithOperatorDetail marks err as holding detail for the operator alone:
// its text is unchanged, and CallerText shows caller in its place.
func WithOperatorDetail(err error, caller string) error {
	if err == nil {
		return nil
	}
	return &operatorError{err: err, caller: caller}
}

// CallerText is the text of err as a probe's caller is answered it: each
// error WithOperatorDetail marked, wherever it is in the chain, replaced by
// what the caller is shown of it, each value of the answer an error made by
// Errorf quotes replaced by WithheldMark (or by the text the value gives
// the caller, QuotedValue.ForCaller), and the text around them, what wraps
// them, as it is. When a wrapper rewrote the text of an error that has
// something to withhold, so that its text is not found in the whole, the
// caller is answered WithheldFailure.
func CallerText(err error) string {
	if err == nil {
		return ""
	}
	text, ok := callerText(err)
	if !ok {
		return WithheldFailure
	}
	return text
}

// callerText is CallerText, and false when what is withheld could not be
// found in the text.
func callerText(err error) (text string, ok bool) {
	// An error that is a nil pointer of its type panics when asked for its
	// text: there is nothing in it to withhold.
	defer func() {
		if recover() != nil {
			text, ok = fmt.Sprint(err), true
		}
	}()
	switch e := err.(type) { //nolint:errorlint // the error itself; those it wraps are walked below
	case *operatorError:
		return e.caller, true
	case *movingError:
		return e.callerText()
	case *sameAsError:
		return callerText(e.err)
	}
	return withCallerParts(err, err.Error())
}

// withCallerParts replaces in text, the text of an error that wraps err, the
// text of each error down err's chain that shows a caller less by what it
// shows: false when one of those is not found in text.
func withCallerParts(err error, text string) (string, bool) {
	switch e := err.(type) { //nolint:errorlint // this is the walk down the chain
	case *operatorError, *movingError, *sameAsError:
		shown, ok := callerText(e)
		if !ok {
			return "", false
		}
		whole := e.Error()
		if shown == whole {
			return text, true
		}
		if !strings.Contains(text, whole) {
			return "", false
		}
		return strings.Replace(text, whole, shown, 1), true
	case interface{ Unwrap() error }:
		if inner := e.Unwrap(); inner != nil {
			return withCallerParts(inner, text)
		}
	case interface{ Unwrap() []error }:
		for _, inner := range e.Unwrap() {
			if inner == nil {
				continue
			}
			var ok bool
			if text, ok = withCallerParts(inner, text); !ok {
				return "", false
			}
		}
	}
	return text, true
}
