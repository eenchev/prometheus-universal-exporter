//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"net/http"

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
			"allowed_targets", "denied_targets", "accept_status",
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
			"accept_status",
		},
		URLPath:     true,
		Validate:    validateHTTPRequest,
		CheckTarget: checkHTTPTarget,
		Fetch: func(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
			return fetch(ctx, target, c, overrides, forwarded)
		},
	})
}
