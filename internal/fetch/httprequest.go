//go:build !select_request_types || request_type_http || request_type_graphite

package fetch

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
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
	if err := checkScheme(c, u.Scheme); err != nil {
		return err
	}
	// url.Parse takes a port of digits however many, which no connection
	// can use: 0, or one past 65535, would fail every scrape. The URL is not
	// shown, as it may carry credentials.
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("has the port %q in its target URL; want a number from 1 to 65535", port)
		}
	}
	return nil
}

// checkHTTPTargetTLS checks a static target's own tls block, which replaces
// the collector's whole for every request of the target's trips
// (withTargetTLS), as the collector's is checked (checkTargetTLS). Beside
// it, the target's insecure_skip_verify, which overrides the block's, would
// leave two answers to one question; the block's own key says it. A block
// on an http:// target is taken, as the collector's is: it is used by the
// https requests the trip makes, a redirect's or a login's.
func checkHTTPTargetTLS(t *model.StaticTarget) error {
	if err := checkTargetTLS(t); err != nil {
		return err
	}
	if t.Request.TLS != nil && t.Request.InsecureSkipVerify != nil {
		return errors.New("sets request.insecure_skip_verify beside a request.tls block of its own; write insecure_skip_verify in the tls block")
	}
	return nil
}

// checkHTTPFiles reads, for --dry-run, the files of the login, the
// collector's or a static target's (checkLoginFiles), and those of a static
// target's own tls block, as its first request would.
func checkHTTPFiles(c *model.Collector, t *model.StaticTarget) []string {
	warnings := checkLoginFiles(c, t)
	if t != nil && t.Request.TLS != nil {
		if _, err := tlsConfig(*t.Request.TLS); err != nil {
			warnings = append(warnings, fmt.Sprintf("target %q: request.tls: %v; a scrape would fail on it now (the files are read at the first request and again when they change, so one put in place later is used then)", t.Name, err))
		}
	}
	return warnings
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
	if err := validateCollectorLogin(x); err != nil {
		return err
	}
	if err := checkForwardHeaders(x.Request.ForwardHeaders); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if err := checkQueryNames(x.Request.Query); err != nil {
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
	for _, entry := range x.Request.RedirectTrustedHosts {
		if _, _, err := parseTrustedHost(entry); err != nil {
			return fmt.Errorf("collector %q %w", x.Name, err)
		}
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

// checkQueryNames refuses a request.query name of nothing, which would be
// sent as a pair with no name, =value, and one holding a control
// character, almost always the line break an environment variable ended
// with. Any other character of a name is sent escaped (url.Values.Encode).
func checkQueryNames(query map[string]string) error {
	for _, name := range model.SortedKeys(query) {
		if name == "" {
			return errors.New("request.query has a parameter without a name; write its name, or leave the parameter out")
		}
		for _, r := range name {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("request.query name %q has the control character %q in it; a parameter's name is sent as written, escaped", name, r)
			}
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
