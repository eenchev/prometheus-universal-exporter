package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
)

// Every collector declares how it reaches its data with request.type. Only
// http exists today; gRPC, local files and FTP are expected. The type is
// required rather than defaulted so that when a second type arrives, no
// existing configuration silently means "http" by accident, and so that each
// type can own its rules:
//
//   - Fields: the request keys it accepts. A key another type owns is an error
//     for this one, so a configuration cannot carry settings that are quietly
//     ignored.
//   - Overrides: the /probe parameters it accepts. A parameter that belongs to
//     some other type is rejected with 400 rather than ignored.
//   - TargetFields: the keys a scheduled target's request block may set.
//   - Validate: the type's required fields, defaults and cross-field rules.
//   - Fetch: how a scrape actually gets its bytes. Everything after it —
//     decoding, transforms, limits, caching — is shared by every type.

// RequestTypeHTTP is the only request type implemented so far.
const RequestTypeHTTP = "http"

type requestType struct {
	Name         string
	Fields       []string
	Overrides    []string // a name ending in "_" is a prefix, such as header_
	TargetFields []string
	Validate     func(c *Collector) error
	Fetch        func(ctx context.Context, target string, c *Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error)
}

// requestTypes is the registry. It is a variable only so a test can register
// a fixture type to exercise the per-type rules with more than one type.
var requestTypes = map[string]*requestType{
	RequestTypeHTTP: {
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
			"header_", pathParamPrefix,
		},
		TargetFields: []string{
			"method", "path", "body", "timeout", "insecure_skip_verify",
			"follow_redirects", "enable_http2", "retry", "headers",
			"basic_auth", "basic_auth_file", "bearer_token", "bearer_token_file",
		},
		Validate: validateHTTPRequest,
		Fetch: func(ctx context.Context, target string, c *Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
			return fetch(ctx, target, c, overrides, forwarded)
		},
	},
}

// supportedRequestTypes lists the registered names for error messages.
func supportedRequestTypes() string {
	names := make([]string, 0, len(requestTypes))
	for name := range requestTypes {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// validateRequest checks a collector's request block: the type is present and
// known, every key set belongs to that type, and the type's own rules hold.
// The keys are checked before the type validates, so a default the type fills
// in, such as method GET, is never mistaken for something the author wrote.
func validateRequest(c *Collector) error {
	c.Request.Type = strings.ToLower(strings.TrimSpace(c.Request.Type))
	if c.Request.Type == "" {
		return fmt.Errorf("collector %q has no request.type; it is required, and the supported types are: %s (add `type: http` to its request block)", c.Name, supportedRequestTypes())
	}
	rt, ok := requestTypes[c.Request.Type]
	if !ok {
		return fmt.Errorf("collector %q has unsupported request.type %q; the supported types are: %s", c.Name, c.Request.Type, supportedRequestTypes())
	}
	for _, key := range setKeys(reflect.ValueOf(c.Request), "type") {
		if !contains(rt.Fields, key) {
			return fmt.Errorf("collector %q sets request.%s, which does not apply to request.type %q", c.Name, key, rt.Name)
		}
	}
	return rt.Validate(c)
}

// requestTypeOf returns the registered type of a validated collector.
func requestTypeOf(c *Collector) *requestType {
	return requestTypes[c.Request.Type]
}

// knownOverrides is every /probe parameter some request type accepts. A
// parameter outside it is not an override at all and is ignored, as it always
// was — a monitor may carry parameters of its own — but one inside it that the
// collector's type does not accept is a mistake worth a 400.
func knownOverrides() []string {
	var all []string
	for _, rt := range requestTypes {
		all = append(all, rt.Overrides...)
	}
	return all
}

// checkOverrideParams rejects probe parameters that belong to a different
// request type than the collector's.
func checkOverrideParams(c *Collector, values url.Values) error {
	rt := requestTypeOf(c)
	if rt == nil {
		return fmt.Errorf("collector %q has no registered request type", c.Name)
	}
	known := knownOverrides()
	var rejected []string
	for key := range values {
		if key == "target" || key == "collector" {
			continue
		}
		if matchesOverride(known, key) && !matchesOverride(rt.Overrides, key) {
			rejected = append(rejected, key)
		}
	}
	if len(rejected) == 0 {
		return nil
	}
	sort.Strings(rejected)
	return fmt.Errorf("probe parameters %s do not apply to collector %q, whose request.type is %q", strings.Join(rejected, ", "), c.Name, rt.Name)
}

// checkTargetRequest rejects a scheduled target request block that sets keys
// its collector's request type does not accept.
func checkTargetRequest(t *ScheduledTarget, c *Collector) error {
	rt := requestTypeOf(c)
	if rt == nil {
		return fmt.Errorf("target %q uses collector %q, which has no registered request type", t.Name, c.Name)
	}
	keys := setKeys(reflect.ValueOf(t.Request))
	if t.Request.PathSet && !contains(keys, "path") {
		keys = append(keys, "path")
	}
	if t.Request.BodySet && !contains(keys, "body") {
		keys = append(keys, "body")
	}
	for _, key := range keys {
		if !contains(rt.TargetFields, key) {
			return fmt.Errorf("target %q sets request.%s, which does not apply to collector %q, whose request.type is %q", t.Name, key, c.Name, rt.Name)
		}
	}
	return nil
}

// fetchCollector gets a scrape's bytes the way the collector's type does.
func fetchCollector(ctx context.Context, target string, c *Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	rt := requestTypeOf(c)
	if rt == nil {
		return nil, fmt.Errorf("collector %q has no registered request type", c.Name)
	}
	return rt.Fetch(ctx, target, c, overrides, forwarded)
}

// setKeys returns the yaml keys of a struct's fields that hold a non-zero
// value, skipping the keys named in skip and fields that are not read from
// YAML at all.
func setKeys(v reflect.Value, skip ...string) []string {
	var keys []string
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		key, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if key == "" || key == "-" || contains(skip, key) {
			continue
		}
		if !v.Field(i).IsZero() {
			keys = append(keys, key)
		}
	}
	return keys
}

func matchesOverride(overrides []string, key string) bool {
	for _, name := range overrides {
		if strings.HasSuffix(name, "_") {
			if strings.HasPrefix(key, name) {
				return true
			}
			continue
		}
		if key == name {
			return true
		}
	}
	return false
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

// validateHTTPRequest holds the http type's rules. Nothing but type is
// required: path may be empty, since the target URL can carry the whole path,
// and method defaults to GET.
func validateHTTPRequest(x *Collector) error {
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
	if hasPathParams(x.Request.Path) {
		if _, err := parsePathParams(x.Request.Path); err != nil {
			return fmt.Errorf("collector %q: %w", x.Name, err)
		}
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
