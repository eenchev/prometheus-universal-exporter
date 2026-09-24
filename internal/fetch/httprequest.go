//go:build !select_request_types || request_type_http || request_type_graphite

package fetch

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The rules of a request made over HTTP, shared by the types that make one:
// http, and graphite, which asks a Graphite render API. They live outside
// either type's file so a build with only one of them still has them, and
// behind both types' tags so a build with neither leaves them out.

// checkHTTPTarget checks a static target's address, which must be an
// absolute URL. A probe's target may be a bare host:port, as Prometheus
// service discovery hands it over, and is checked when it is fetched.
func checkHTTPTarget(_ *model.Collector, target string, static bool) error {
	if !static {
		return nil
	}
	if u, err := url.Parse(target); err != nil || u.Host == "" {
		return errors.New("must have an absolute target URL")
	}
	return nil
}

// validateHTTPRequest holds the http type's rules. Nothing but type is
// required: path may be empty, since the target URL can carry the whole path,
// and method defaults to GET.
func validateHTTPRequest(x *model.Collector) error {
	if x.Request.BearerToken != "" && x.Request.BearerTokenFile != "" {
		return fmt.Errorf("collector %q cannot set both request.bearer_token and request.bearer_token_file", x.Name)
	}
	if x.Request.BasicAuth != nil && x.Request.BasicAuthFile != nil {
		return fmt.Errorf("collector %q cannot set both request.basic_auth and request.basic_auth_file", x.Name)
	}
	if x.Request.BasicAuthFile != nil && (strings.TrimSpace(x.Request.BasicAuthFile.Username) == "" || strings.TrimSpace(x.Request.BasicAuthFile.Password) == "") {
		return fmt.Errorf("collector %q basic_auth_file requires username and password paths", x.Name)
	}
	if x.Request.Retry.Attempts < 0 {
		return fmt.Errorf("collector %q request.retry.attempts must not be negative", x.Name)
	}
	if x.Request.Retry.Backoff < 0 {
		return fmt.Errorf("collector %q request.retry.backoff must not be negative", x.Name)
	}
	if (x.Request.BasicAuth != nil || x.Request.BasicAuthFile != nil) && (x.Request.BearerToken != "" || x.Request.BearerTokenFile != "") {
		return fmt.Errorf("collector %q cannot configure basic and bearer authentication together", x.Name)
	}
	if HasPathParams(x.Request.Path) {
		if _, err := parsePathParams(x.Request.Path); err != nil {
			return fmt.Errorf("collector %q: %w", x.Name, err)
		}
	}
	if err := validateRequestTemplates(x); err != nil {
		return err
	}
	if x.Request.Method == "" {
		x.Request.Method = http.MethodGet
	}
	x.Request.Method = strings.ToUpper(x.Request.Method)
	switch x.Request.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead:
	default:
		return fmt.Errorf("collector %q has unsupported method %q", x.Name, x.Request.Method)
	}
	return nil
}

// validateRequestTemplates checks the templated fields of an http collector
// when the configuration loads: placeholders well formed, filters known, and
// no placeholder in a header or query name.
func validateRequestTemplates(c *model.Collector) error {
	for name := range c.Request.Headers {
		if strings.Contains(name, "{{") {
			return fmt.Errorf("collector %q request.headers name %q has a placeholder; placeholders stand only in header values", c.Name, name)
		}
	}
	for name := range c.Request.Query {
		if strings.Contains(name, "{{") {
			return fmt.Errorf("collector %q request.query name %q has a placeholder; placeholders stand only in query values", c.Name, name)
		}
	}
	for _, f := range requestTemplates(c, RequestOverrides{}) {
		if _, err := f.parse(); err != nil {
			return fmt.Errorf("collector %q: %w", c.Name, err)
		}
	}
	return nil
}
