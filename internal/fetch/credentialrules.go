//go:build !select_request_types || request_type_http || request_type_graphite || request_type_grpc

package fetch

import (
	"errors"
	"fmt"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The credential and TLS keys' rules, for the types that reach a server:
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
