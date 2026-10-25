//go:build !select_request_types || request_type_http

package fetch

import (
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
			"follow_redirects", "redirect_trusted_hosts", "enable_http2", "allowed_schemes",
			"allowed_targets", "denied_targets", "accept_status", "pagination", "login", "rate_limit",
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
			"accept_status", "login", "rate_limit", "tls",
		},
		URLPath:     true,
		Revalidates: true,
		DebugPage:   true,
		Validate: func(c *model.Collector) error {
			if err := validateHTTPRequest(c); err != nil {
				return err
			}
			return validatePagination(c)
		},
		CheckTarget: checkHTTPTarget,
		CheckTargetRequest: func(c *model.Collector, t *model.StaticTarget) error {
			if err := checkTargetLogin(c, t); err != nil {
				return err
			}
			return checkHTTPTargetTLS(t)
		},
		Fetch:      fetchHTTP,
		TargetHint: func(*model.Collector) string { return "http://host:port" },
		// The files of a login and of a target's own tls block; the
		// tokens request.login keeps, and its counts (login.go).
		CheckFiles:       checkHTTPFiles,
		Follow:           logins.follow,
		CloseConnections: logins.reset,
		Logins:           logins.counts,
	})
}
