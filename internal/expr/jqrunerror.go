package expr

import "github.com/eenchev/prometheus-universal-exporter/internal/model"

// JQRunError is err, an error a jq program ended with as it ran on the
// answer, marked as the operator's (model.WithOperatorDetail): gojq writes
// the start of the value it failed on into its errors, as in `cannot iterate
// over: string ("...")`, and a program's own error(...) may hold anything of
// the data. A probe's caller is told the program failed, and the exporter's
// log, the debug report and, with --web.debug=full, the status page have why.
func JQRunError(err error) error {
	return model.WithOperatorDetail(err, "the jq program failed on the answer: "+model.WithheldMark)
}
