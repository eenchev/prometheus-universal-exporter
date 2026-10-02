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
func checkHTTPTarget(c *model.Collector, target string, static bool) error {
	if !static {
		return nil
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return errors.New("must have an absolute target URL")
	}
	// A probe's target is checked for its scheme when it is fetched; a
	// static target's is known now, and one that is not allowed would fail
	// every scrape.
	return checkScheme(c, u.Scheme)
}

// validateHTTPRequest holds the http type's rules. Nothing but type is
// required: path may be empty, since the target URL can carry the whole path,
// and method defaults to GET.
func validateHTTPRequest(x *model.Collector) error {
	if err := validateCredentials(x); err != nil {
		return err
	}
	if err := checkTLSSettings(x.Request.TLS); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if x.Request.Retry.Attempts < 0 || x.Request.Retry.Attempts > MaxRetryAttempts {
		return fmt.Errorf("collector %q request.retry.attempts must be from 0 to %d", x.Name, MaxRetryAttempts)
	}
	if x.Request.Retry.Backoff < 0 {
		return fmt.Errorf("collector %q request.retry.backoff must not be negative", x.Name)
	}
	if err := checkAllowedSchemes(x.Request.AllowedSchemes); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if HasPathParams(x.Request.Path) {
		placeholders, err := parsePathParams(x.Request.Path)
		if err != nil {
			return fmt.Errorf("collector %q: %w", x.Name, err)
		}
		// A default no probe could be sent with (bindPathParams) is refused
		// now, rather than by every probe that leaves the parameter out.
		if err := checkPathParamDefaults(x.Name, placeholders, checkPathParamValue); err != nil {
			return err
		}
	}
	if err := checkURLPath(x.Request.Path); err != nil {
		return fmt.Errorf("collector %q request.path %w", x.Name, err)
	}
	if err := checkHeaderNames(x.Request.Headers); err != nil {
		return fmt.Errorf("collector %q request.headers %w", x.Name, err)
	}
	for _, name := range model.SortedKeys(x.Request.Headers) {
		if value := x.Request.Headers[name]; !HasPathParams(value) {
			if err := checkHeaderValue(value); err != nil {
				return fmt.Errorf("collector %q request.headers %s %w", x.Name, name, err)
			}
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
		if err := f.check(); err != nil {
			return fmt.Errorf("collector %q: %w", c.Name, err)
		}
	}
	return nil
}

// checkAllowedSchemes checks request.allowed_schemes at load. A request is
// made over http or https and nothing else, so any other entry is a typing
// mistake, such as htps, that would refuse every target the collector is
// given; so would an entry with a space around it, or the :// of a URL,
// which matches no scheme.
func checkAllowedSchemes(schemes []string) error {
	for i, scheme := range schemes {
		if !strings.EqualFold(scheme, "http") && !strings.EqualFold(scheme, "https") {
			return fmt.Errorf("request.allowed_schemes[%d] is %q; an entry is http or https, written without spaces or ://", i, scheme)
		}
	}
	return nil
}
