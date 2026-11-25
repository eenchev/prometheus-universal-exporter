//go:build !select_request_types || request_type_http || request_type_graphite || request_type_grpc || request_type_sql

package fetch

import "fmt"

// The load-time rule of a templated field, for the types that have one. In
// http and graphite these are the body, the header and query values and the
// Graphite expressions; in grpc, the message and the metadata values; in
// sql, the database's name. localfile has placeholders only in its path, so
// a build with only it leaves this out.

// check is what a field is held to when the configuration loads: its
// placeholders must be well formed, and each default must be writable where
// it stands. Some defaults their own place refuses: ten under |number, a
// glob in a Graphite expression, a line break in a header. Such a default
// could never be sent, so every probe that left the parameter out would fail
// with a 400 that blames the probe.
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
			if p.Clock != nil {
				return fmt.Errorf("%w; the clock placeholder writes values such as that one, so every probe would fail — choose another format or filter", err)
			}
			return fmt.Errorf("%w; that value is the placeholder's default, so every probe that leaves %s out would fail — change the default", err, p.Name)
		}
	}
	return nil
}
