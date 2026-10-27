//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The http request type: a GET, or another method, against the target URL.
// It is included in every default build; with -tags select_request_types it
// is included only when request_type_http is among the tags.

// HTTPConfig is an http collector's own request keys: the method, GET
// when left out; the body sent; and the pagination that fetches the
// further pages of a paginated answer and merges them into one JSON
// document, the first page with its items array holding every page's items
// (pagination.go), nil when the answer is one page.
type HTTPConfig struct {
	Method     string                  `yaml:"method"`
	Body       string                  `yaml:"body"`
	Pagination *model.PaginationConfig `yaml:"pagination,omitempty"`
}

// httpRequest is an http collector's own request keys: the zero HTTPConfig
// of a collector built in code whose request has not been validated, which
// sets them (ValidateRequest).
func httpRequest(c *model.Collector) HTTPConfig {
	own, _ := c.Request.Own.(HTTPConfig)
	return own
}

func init() {
	registerRequestType(&RequestType{
		Name: RequestTypeHTTP,
		Fields: []string{
			"path", "query", "headers",
			"basic_auth", "basic_auth_file", "bearer_token", "bearer_token_file",
			"forward_authorization", "forward_headers",
			"tls", "retry", "max_response_bytes",
			"follow_redirects", "redirect_trusted_hosts", "enable_http2", "allowed_schemes",
			"allowed_targets", "denied_targets", "accept_status", "login", "rate_limit",
			"proxy_url", "proxy_basic_auth_file",
		},
		Config:   HTTPConfig{},
		KeyRules: httpKeyRules,
		MethodBody: func(c *model.Collector) (method, body string) {
			own := httpRequest(c)
			return own.Method, own.Body
		},
		Policy: func(c *model.Collector) RequestPolicy {
			return RequestPolicy{Pagination: httpRequest(c).Pagination}
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
		Requester:   httpRequester{},
		URLPath:     true,
		Revalidates: true,
		DebugPage:   true,
		CheckTargetRequest: func(c *model.Collector, t *model.StaticTarget) error {
			if err := checkTargetLogin(c, t); err != nil {
				return err
			}
			return checkHTTPTargetTLS(t)
		},
		TargetHint: func(*model.Collector) string { return "http://host:port" },
		// The files of a login and of a target's own tls block; the
		// tokens request.login keeps, and its counts (login.go).
		CheckFiles:       checkHTTPFiles,
		Follow:           logins.follow,
		CloseConnections: logins.reset,
		Logins:           logins.counts,
	})
}

// httpRequester is the http request type's Requester. Its target is a URL
// (URLTarget).
type httpRequester struct{ URLTarget }

// Validate checks an http collector's request and fills in its defaults:
// the rules of a request made over HTTP, then its method, GET when left
// out and in upper case, and its pagination.
func (httpRequester) Validate(c *model.Collector) error {
	if err := validateHTTPRequest(c); err != nil {
		return err
	}
	own := httpRequest(c)
	if own.Method == "" {
		own.Method = http.MethodGet
	}
	own.Method = strings.ToUpper(own.Method)
	c.Request.Own = own
	switch own.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead:
	default:
		return fmt.Errorf("collector %q has unsupported method %q", c.Name, own.Method)
	}
	return validatePagination(c)
}

// Fetch performs one request of an http collector.
func (httpRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	return fetchHTTP(ctx, target, c, overrides, forwarded)
}

// CheckTarget checks an http collector's target.
func (httpRequester) CheckTarget(c *model.Collector, target string, static bool) error {
	return checkHTTPTarget(c, target, static)
}

// httpKeyRules is what the configuration schema says of a http
// collector's own keys.
func httpKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"method":               {"enum": SchemaOptionalEnum([]string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "get", "post", "put", "patch", "delete", "head"}), "description": "HTTP method. Defaults to GET."},
		"body":                 {"description": "http: the request body. May contain {{param_name}} placeholders, written as |json, |number, |form, |xml or |raw: {{param_service|json}}. See docs/REQUESTS.md#in-the-body-headers-and-query."},
		"pagination":           httpPaginationRule(),
		"pagination.type":      {"enum": model.PaginationTypes, "description": "Required. How the next page is found. link: the URL of the Link header's rel=\"next\" (RFC 8288). next_url: the URL next reads from the page. cursor: the value next reads from the page, sent as the query parameter param. page: a page number in param, from start, one more each page. offset: an offset in param, from start, moved on by the items each page holds, or by size."},
		"pagination.items":     {"not": SchemaOnlyBlanks(), "description": "Required. The jq path of the array each page holds its items in, such as .items, .data.results, or . for a page that is an array. The merged answer is the first page with this array holding the items of every page, in order."},
		"pagination.next":      {"description": "Required with type next_url and cursor, and for no other: the jq expression of the next page's URL, absolute or relative to the page's, or of its cursor, read from the page, with $headers and $status as in a rule. null, false or \"\" ends the pages."},
		"pagination.param":     {"description": "Required with type cursor, page and offset, and for no other: the query parameter the cursor, the page number or the offset is sent in. It replaces a parameter of that name in the target's query; request.query may not set it."},
		"pagination.start":     {"description": "page and offset: the first page's number, 1 when left out, or offset, 0 when left out. The first page is asked for with it."},
		"pagination.size":      {"minimum": 1, "description": "offset: how far the offset moves from one page to the next. Left out, by the number of items the page held."},
		"pagination.max_pages": {"minimum": 1, "maximum": model.MaxMaxPages, "description": "The most pages one trip fetches, the first included: from 1 to 1000, 10 when left out. A trip that stops here with another page to come is answered with the pages it fetched, counted in http_exporter_pagination_max_pages_reached_total and logged."},
	}
}

// httpPaginationRule is the rule of request.pagination: type and items
// required, each written with something other than "", next with type
// next_url and cursor, and param with cursor, page and offset, as the
// exporter has them (fetch.validatePagination).
func httpPaginationRule() map[string]any {
	when := func(types ...string) map[string]any {
		return map[string]any{"properties": map[string]any{"type": map[string]any{"enum": types}}, "required": []string{"type"}}
	}
	return map[string]any{
		"description": "http: fetch the further pages of a paginated answer and merge them into one JSON document, the first page with its items array holding every page's items, which the decoder and the rules read as the answer. Every page is sent the request's headers, credentials and body, and must stay on the target's scheme, host and port. The decoder is json. See docs/REQUESTS.md#pagination.",
		"allOf": []any{
			SchemaWrittenKey("type"),
			SchemaWrittenKey("items"),
			map[string]any{"if": when(model.PaginationNextURL, model.PaginationCursor), "then": SchemaWrittenKey("next")},
			map[string]any{"if": when(model.PaginationCursor, model.PaginationPage, model.PaginationOffset), "then": SchemaWrittenKey("param")},
		},
	}
}
