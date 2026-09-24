//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The http request type: a GET, or another method, against the target URL.
// It is included in every default build; with -tags select_request_types it
// is included only when request_type_http is among the tags.

func init() {
	registerRequestType(&RequestType{
		Name: RequestTypeHTTP,
		Fields: []string{
			"method", "path", "query", "headers", "body",
			"basic_auth", "basic_auth_file", "bearer_token", "bearer_token_file",
			"forward_authorization", "forward_headers",
			"tls", "retry", "max_response_bytes",
			"follow_redirects", "enable_http2", "allowed_schemes",
		},
		Overrides: []string{
			"method", "path", "timeout", "body", "insecure_skip_verify",
			"follow_redirects", "enable_http2", "retry_attempts", "retry_backoff",
			"header_", PathParamPrefix,
		},
		TargetFields: []string{
			"method", "path", "body", "timeout", "insecure_skip_verify",
			"follow_redirects", "enable_http2", "retry", "headers",
			"basic_auth", "basic_auth_file", "bearer_token", "bearer_token_file",
		},
		Validate:    validateHTTPRequest,
		CheckTarget: checkHTTPTarget,
		Fetch: func(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
			return fetch(ctx, target, c, overrides, forwarded)
		},
	})
}

// checkHTTPTarget checks a scheduled target's address, which must be an
// absolute URL. A probe's target may be a bare host:port, as Prometheus
// service discovery hands it over, and is checked when it is fetched.
func checkHTTPTarget(_ *model.Collector, target string, scheduled bool) error {
	if !scheduled {
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
