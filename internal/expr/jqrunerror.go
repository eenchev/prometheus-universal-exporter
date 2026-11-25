package expr

import "github.com/eenchev/prometheus-universal-exporter/internal/model"

// JQRunError wraps err, an error that a jq program ended with while running
// on the answer, and marks it as the operator's (model.WithOperatorDetail).
// gojq writes the start of the value that it failed on into its errors, as
// in `cannot iterate over: string ("...")`, and a program's own error(...)
// may hold any part of the data. A probe's caller is told that the program
// failed; the exporter's log, the debug report and, with --web.debug=full,
// the status page say why.
func JQRunError(err error) error {
	return model.WithOperatorDetail(err, "the jq program failed on the answer: "+model.WithheldMark)
}
