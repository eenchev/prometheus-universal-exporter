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
