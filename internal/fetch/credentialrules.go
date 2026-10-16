//go:build !select_request_types || request_type_http || request_type_graphite || request_type_grpc

package fetch

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"golang.org/x/net/http/httpguts"
)

// The credential, header forwarding and TLS keys' rules, for the types that
// reach a server:
// http and graphite, which send credentials as an Authorization header, and
// grpc, which sends them as the authorization metadata. localfile reaches no
// server, so a build with only it leaves them out.

// validateCredentials refuses credential keys that contradict each other.
func validateCredentials(x *model.Collector) error {
	if x.Request.BearerToken != "" && x.Request.BearerTokenFile != "" {
		return fmt.Errorf("collector %q cannot set both request.bearer_token and request.bearer_token_file", x.Name)
	}
	if x.Request.BasicAuth != nil && x.Request.BasicAuthFile != nil {
		return fmt.Errorf("collector %q cannot set both request.basic_auth and request.basic_auth_file", x.Name)
	}
	if x.Request.BasicAuthFile != nil && (strings.TrimSpace(x.Request.BasicAuthFile.Username) == "" || strings.TrimSpace(x.Request.BasicAuthFile.Password) == "") {
		return fmt.Errorf("collector %q basic_auth_file requires username and password paths", x.Name)
	}
	if (x.Request.BasicAuth != nil || x.Request.BasicAuthFile != nil) && (x.Request.BearerToken != "" || x.Request.BearerTokenFile != "") {
		return fmt.Errorf("collector %q cannot configure basic and bearer authentication together", x.Name)
	}
	if err := checkInlineCredentials(x.Request.BearerToken, x.Request.BasicAuth); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	return nil
}

// checkTLSSettings refuses, when the configuration loads, a tls block that
// could never make a connection: a client certificate without its key, or a
// key without its certificate. The files themselves are read at the first
// request, since a mounted Secret may be replaced after the exporter starts.
func checkTLSSettings(t model.TLSConfig) error {
	if (t.CertFile == "") != (t.KeyFile == "") {
		return errors.New("request.tls sets only one of cert_file and key_file; a client certificate needs both")
	}
	return nil
}

// checkForwardHeaders refuses an entry of request.forward_headers that
// would never be forwarded: one that is no header name, written with blanks
// around it or not, as a probe's forwarding trims them, and one of the
// headers a probe never forwards (UnforwardableReason). Either was taken, and
// the header listed silently never reached the target.
func checkForwardHeaders(names []string) error {
	for _, written := range names {
		name := strings.TrimSpace(written)
		if !httpguts.ValidHeaderFieldName(name) {
			return fmt.Errorf("request.forward_headers has %q, which is not a header name; a name is letters, digits and !#$%%&'*+-.^_`|~, without spaces", written)
		}
		if reason := UnforwardableReason(http.CanonicalHeaderKey(name)); reason != "" {
			return fmt.Errorf("request.forward_headers has %q, which is never forwarded: %s", written, reason)
		}
	}
	return nil
}
