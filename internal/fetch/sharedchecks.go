package fetch

import (
	"errors"
	"fmt"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The exporter sends requests of its own besides its collectors', such as
// the OTLP export's. Their headers and TLS settings are held to what a
// collector's request is held to when the configuration loads, by the same
// code, so a setting Go would refuse to send is a load error for both rather
// than a failure of every request. These are built with every request type.

// CheckHeaders applies the checks of a collector's request.headers to headers
// that take no placeholders: every name is one Go sends, none is set twice
// in different case, and no value holds a control character.
func CheckHeaders(headers map[string]string) error {
	for _, name := range model.SortedKeys(headers) {
		// checkHeaderNames leaves a name with a placeholder to the check of
		// a collector's templates; here there are none, so it is no name.
		if strings.Contains(name, "{{") {
			return fmt.Errorf("%q is not a header name; a name is letters, digits and !#$%%&'*+-.^_`|~, without spaces", name)
		}
	}
	if err := checkHeaderNames(headers); err != nil {
		return err
	}
	for _, name := range model.SortedKeys(headers) {
		if err := checkHeaderValue(headers[name]); err != nil {
			return fmt.Errorf("%s %w", name, err)
		}
	}
	return nil
}

// CheckClientTLS refuses a tls block that names a client certificate without
// its key, or a key without its certificate, as a collector's request.tls is
// refused, and one whose files cannot be read or hold no certificate or key:
// unlike a collector's, which are read at the first request to a target,
// these are needed from the start, by a request the exporter makes on its own.
func CheckClientTLS(t model.TLSConfig) error {
	if (t.CertFile == "") != (t.KeyFile == "") {
		return errors.New("sets only one of cert_file and key_file; a client certificate needs both")
	}
	if _, err := tlsConfig(t); err != nil {
		return fmt.Errorf("cannot be used: %w", err)
	}
	return nil
}

// TemplatedFields names the fields of a collector's request whose
// {{param_...}} placeholders a probe's parameters fill, as the request's own
// errors name them: request.path, and what requestTemplates renders, such as
// request.body and request.headers.<name>. It is how the configuration knows
// that a placeholder anywhere else would be used as written, without a
// second list of where placeholders work to keep in step with this one.
func TemplatedFields(c *model.Collector) []string {
	var fields []string
	if HasPathParams(c.Request.Path) {
		fields = append(fields, "request.path")
	}
	for _, field := range requestTemplates(c, RequestOverrides{}) {
		fields = append(fields, field.where)
	}
	return fields
}
