//go:build !select_request_types || request_type_http || request_type_graphite || request_type_grpc

package fetch

import "fmt"

// The load-time rule of a templated field, for the types that have one: http
// and graphite, in the body, the header and query values and the Graphite
// expressions, and grpc, in the message and the metadata values. localfile
// has placeholders in its path alone, so a build with only it leaves this
// out.

// check is what a field is held to when the configuration loads: its
// placeholders well formed, and each default one that can be written where
// it stands. A default its own place refuses — ten under |number, a glob in
// a Graphite expression, a line break in a header — could never be sent, so
// every probe that left the parameter out would fail with a 400 that blames
// the probe.
func (f templateField) check() error {
	placeholders, err := f.parse()
	if err != nil {
		return err
	}
	for _, p := range placeholders {
		if !p.HasDefault {
			continue
		}
		if _, err := f.write(p, p.Default); err != nil {
			return fmt.Errorf("%w; that value is the placeholder's default, so every probe that leaves %s out would fail — change the default", err, p.Name)
		}
	}
	return nil
}
