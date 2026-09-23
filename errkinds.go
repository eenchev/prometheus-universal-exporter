package main

import "errors"

// Some failures are counted in their own self-metrics: a response or file over
// the size limit (http_exporter_series_limit_exceeded_total), a metric value
// the response did not contain (http_exporter_missing_keys_total) and a Python
// script that failed (http_exporter_script_errors_total). Which counter an
// error raises is decided by what the error is, marked where it is made, and
// read with errors.Is — never by searching its text, which a reworded message
// or an unrelated error that happens to say "missing" would get wrong.
var (
	errLimitExceeded = errors.New("limit exceeded")
	errMissingValue  = errors.New("value missing")
	errScriptFailed  = errors.New("script failed")
)

// kindError marks an error with its kind while keeping its message and chain.
type kindError struct {
	err  error
	kind error
}

func (e *kindError) Error() string        { return e.err.Error() }
func (e *kindError) Unwrap() error        { return e.err }
func (e *kindError) Is(target error) bool { return target == e.kind }

// markError marks err as a failure of the given kind. The message is unchanged.
func markError(err, kind error) error {
	if err == nil {
		return nil
	}
	return &kindError{err: err, kind: kind}
}
