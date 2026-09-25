//go:build !select_request_types || request_type_graphite

package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The graphite request type asks a Graphite render API — graphite-web,
// carbonapi, or anything that answers /render the same way — for the series
// of one or more Graphite expressions:
//
//	request:
//	  type: graphite
//	  targets:                    # required: each sent as a target parameter
//	    - app.*.requests.count
//	    - "seriesByTag('name=cpu.load', 'env={{param_env:prod}}')"
//	  from: -15min                # optional, the default
//	  until: now                  # optional, the default
//
// The probe's target is the Graphite server, as an http collector's is its
// host, and the request is a GET of target + path (/render by default) with
// target=, from=, until= and format=json — or, when the expressions are too
// long for a URL that every proxy passes, a POST of the same parameters as a
// form, which graphite-web and carbonapi read alike. Everything about the connection —
// authentication, TLS, retries, redirects, the response limit, the proxy
// from the environment, the shared connection pool — is http's, since it is
// the same request: only the URL is built differently, because request.query
// holds one value to a name and Graphite takes target once per expression.
//
// What comes back is decoded by the graphite decoder (decode/graphite.go),
// which decoder.type auto picks for this type, into series that jq, yq or a
// Python script turn into metrics.
//
// Pushing to the exporter is not supported: carbon's line protocol is a push
// protocol, and the exporter only pulls. The graphite decoder reads a file of
// carbon lines through a localfile collector instead.

// graphiteOwnParams are the query parameters the type sets itself, so
// request.query cannot set them too.
var graphiteOwnParams = []string{"target", "from", "until", "format"}

// The render window used when request.from and request.until are left out.
// Fifteen minutes holds a few points of a series stored at one-minute or
// five-minute resolution, and the last complete point of one a statsd flush
// writes late; response.graphite.max_age says how old the newest may be.
const (
	graphiteDefaultFrom  = "-15min"
	graphiteDefaultUntil = "now"
)

// graphitePostAbove is how many bytes of encoded expressions a request may
// carry in its URL; past it, it is sent as a form with POST. Proxies and
// servers commonly refuse URLs of a few kilobytes, and this leaves the rest
// of the URL room.
const graphitePostAbove = 2048

func init() {
	registerRequestType(&RequestType{
		Name: RequestTypeGraphite,
		Fields: []string{
			"path", "query", "headers",
			"basic_auth", "basic_auth_file", "bearer_token", "bearer_token_file",
			"forward_authorization", "forward_headers",
			"tls", "retry", "max_response_bytes",
			"follow_redirects", "enable_http2", "allowed_schemes",
			"targets", "from", "until",
			"allowed_targets", "denied_targets", "accept_status",
		},
		Overrides: []string{
			"path", "timeout", "insecure_skip_verify",
			"follow_redirects", "enable_http2", "retry_attempts", "retry_backoff",
			"header_", PathParamPrefix, "from", "until",
		},
		TargetFields: []string{
			"path", "timeout", "insecure_skip_verify",
			"follow_redirects", "enable_http2", "retry", "headers",
			"basic_auth", "basic_auth_file", "bearer_token", "bearer_token_file",
			"targets", "from", "until",
			"accept_status",
		},
		URLPath:            true,
		Validate:           validateGraphiteRequest,
		CheckTarget:        checkHTTPTarget,
		Query:              graphiteQuery,
		CheckTargetRequest: checkGraphiteTargetRequest,
		Method:             graphiteMethod,
		Fetch: func(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
			overrides.formPost = graphiteMethod(c, overrides) == http.MethodPost
			return fetch(ctx, target, c, overrides, forwarded)
		},
	})
}

// validateGraphiteRequest holds the graphite type's rules: at least one
// expression, each well formed; a render window without spaces; no query
// parameter the type sets itself; and http's rules for the connection. It
// fills in the defaults: path /render, from -15min, until now. The method is
// always GET, and is left unset, since request.method is not graphite's to
// set: a configuration validated again must not read as one that sets it.
func validateGraphiteRequest(x *model.Collector) error {
	if len(x.Request.Targets) == 0 {
		return fmt.Errorf("collector %q has no request.targets; a graphite collector asks the render API for the series of at least one Graphite expression, such as app.*.requests.count", x.Name)
	}
	for _, name := range model.SortedKeys(x.Request.Query) {
		for _, own := range graphiteOwnParams {
			if strings.EqualFold(name, own) {
				return fmt.Errorf("collector %q sets request.query %s, which a graphite collector sets itself: target from request.targets, from and until from request.from and request.until, and format always json", x.Name, name)
			}
		}
	}
	for _, window := range []struct {
		key   string
		value *string
	}{{"from", &x.Request.From}, {"until", &x.Request.Until}} {
		*window.value = strings.TrimSpace(*window.value)
		if err := checkGraphiteWindow(*window.value); err != nil {
			return fmt.Errorf("collector %q request.%s: %w", x.Name, window.key, err)
		}
	}
	if x.Request.Path == "" {
		x.Request.Path = "/render"
	}
	if x.Request.From == "" {
		x.Request.From = graphiteDefaultFrom
	}
	if x.Request.Until == "" {
		x.Request.Until = graphiteDefaultUntil
	}
	// The connection's rules, and the placeholders of the path, the headers,
	// the query and the targets, are http's.
	if err := validateHTTPRequest(x); err != nil {
		return err
	}
	x.Request.Method = ""
	for i, expression := range x.Request.Targets {
		if err := checkGraphiteExpression(withoutPlaceholders(expression)); err != nil {
			return fmt.Errorf("collector %q request.targets[%d] %q: %w", x.Name, i, expression, err)
		}
	}
	if err := refuseRepeatedExpressions(x.Request.Targets); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	return nil
}

// refuseRepeatedExpressions refuses an expression listed twice, which asks
// Graphite for every one of its series twice.
func refuseRepeatedExpressions(targets []string) error {
	first := map[string]int{}
	for i, expression := range targets {
		expression = strings.TrimSpace(expression)
		if j, seen := first[expression]; seen {
			return fmt.Errorf("request.targets[%d] repeats request.targets[%d], %q; list each expression once", i, j, expression)
		}
		first[expression] = i
	}
	return nil
}

// checkGraphiteTargetRequest checks what a static target's request sets for
// a graphite collector: its own expressions and window, which replace the
// collector's.
func checkGraphiteTargetRequest(_ *model.Collector, t *model.StaticTarget) error {
	for i, expression := range t.Request.Targets {
		if err := checkGraphiteExpression(expression); err != nil {
			return fmt.Errorf("request.targets[%d] %q: %w", i, expression, err)
		}
	}
	if err := refuseRepeatedExpressions(t.Request.Targets); err != nil {
		return err
	}
	for _, window := range [][2]string{{"from", t.Request.From}, {"until", t.Request.Until}} {
		if err := checkGraphiteWindow(strings.TrimSpace(window[1])); err != nil {
			return fmt.Errorf("request.%s: %w", window[0], err)
		}
	}
	return nil
}

// graphiteMethod is GET, or POST when the expressions the request carries,
// encoded, are longer than graphitePostAbove. It is decided by the
// expressions as configured, placeholders unfilled, so one collector or
// target always uses one method, which the http_method label shows.
func graphiteMethod(c *model.Collector, overrides RequestOverrides) string {
	targets := overrides.Targets
	if targets == nil {
		targets = c.Request.Targets
	}
	size := 0
	for _, expression := range targets {
		size += len("&target=") + len(url.QueryEscape(expression))
	}
	if size > graphitePostAbove {
		return http.MethodPost
	}
	return http.MethodGet
}

// graphiteQuery is the query a graphite request adds: every expression as a
// target, the window, and format=json. A static target's targets, from and
// until replace the collector's; the collector's targets have their
// placeholders filled in.
func graphiteQuery(c *model.Collector, overrides RequestOverrides) (url.Values, error) {
	targets := overrides.Targets
	if targets == nil {
		targets = make([]string, 0, len(c.Request.Targets))
		for i, expression := range c.Request.Targets {
			if HasPathParams(expression) {
				rendered, err := templateField{fmt.Sprintf("request.targets[%d]", i), expression, "graphite"}.render(overrides.Params)
				if err != nil {
					return nil, err
				}
				expression = rendered
			}
			targets = append(targets, expression)
		}
	}
	from, until := c.Request.From, c.Request.Until
	if overrides.From != "" {
		from = strings.TrimSpace(overrides.From)
	}
	if overrides.Until != "" {
		until = strings.TrimSpace(overrides.Until)
	}
	return url.Values{"target": targets, "from": {from}, "until": {until}, "format": {"json"}}, nil
}

// withoutPlaceholders stands a plain value in for each {{param_...}}
// placeholder, so an expression is checked as it will be sent: a value may
// hold nothing that changes the expression's structure.
func withoutPlaceholders(expression string) string {
	placeholders, err := parsePlaceholders("", expression, false, false)
	if err != nil || len(placeholders) == 0 {
		return expression
	}
	var b strings.Builder
	previous := 0
	for _, p := range placeholders {
		b.WriteString(expression[previous:p.start])
		b.WriteString("x")
		previous = p.end
	}
	b.WriteString(expression[previous:])
	return b.String()
}

// checkGraphiteWindow refuses a from or until Graphite could not read as one
// value: it takes forms such as -5min, now, 1727000000 or 18:00_20260924,
// none of which has a space in it.
func checkGraphiteWindow(window string) error {
	if strings.IndexFunc(window, unicode.IsSpace) >= 0 {
		return fmt.Errorf("%q has a space in it; write a Graphite time such as -5min, now or 1727000000", window)
	}
	return nil
}

// checkGraphiteExpression checks that an expression is one Graphite could
// parse as a whole: not empty, its quotes closed and its brackets balanced.
// Inside quotes, a backslash escapes the character after it, as Graphite's
// grammar has it: 'it\'s' is one string.
// It does not know Graphite's functions, which differ between graphite-web
// and carbonapi; an unknown function is the render API's to refuse.
func checkGraphiteExpression(expression string) error {
	if strings.TrimSpace(expression) == "" {
		return errors.New("is empty; write a Graphite expression, such as app.*.requests.count")
	}
	closers := map[rune]rune{'(': ')', '[': ']', '{': '}'}
	var open []rune
	var quote rune
	escaped := false
	for _, r := range expression {
		switch {
		case r < 0x20 || r == 0x7f:
			return errors.New("has a control character in it")
		case escaped:
			escaped = false
		case quote != 0 && r == '\\':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case closers[r] != 0:
			open = append(open, r)
		case r == ')' || r == ']' || r == '}':
			if len(open) == 0 || closers[open[len(open)-1]] != r {
				return fmt.Errorf("has a %c that closes nothing; check the brackets", r)
			}
			open = open[:len(open)-1]
		}
	}
	if quote != 0 {
		return fmt.Errorf("has a %c that is never closed", quote)
	}
	if len(open) > 0 {
		return fmt.Errorf("has a %c that is never closed", open[len(open)-1])
	}
	return nil
}
